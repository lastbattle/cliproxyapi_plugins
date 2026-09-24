package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	rule, ok := matchingRule(cfg, req.SourceFormat, req.RequestedModel)
	if !ok {
		logHostFn(req.HostCallbackID, "debug", "model-fallback-router: declined request", map[string]any{
			"requested_model": strings.TrimSpace(req.RequestedModel),
			"source_format":   normalizeProtocol(req.SourceFormat),
			"reason":          "no matching fallback rule",
		})
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	logHostFn(req.HostCallbackID, "debug", "model-fallback-router: claimed request", map[string]any{
		"requested_model": strings.TrimSpace(req.RequestedModel),
		"source_format":   normalizeProtocol(req.SourceFormat),
		"rule":            rule.Name,
	})
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetExecutor,
		Target:     pluginIdentifier,
		Reason:     pluginIdentifier + ":matched",
	})
}

var executeHostModelAttempt = executeHostModel

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	payload, headers, metadata, errRun := runExecutionFallback(req.ExecutorRequest, req.HostCallbackID)
	if errRun != nil {
		return errorEnvelopeWithStatus("executor_error", errRun.Error(), statusOrDefault(statusFromError(errRun))), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload, Headers: headers, Metadata: metadata})
}

func runExecutionFallback(exec pluginapi.ExecutorRequest, hostCallbackID string) ([]byte, http.Header, map[string]any, error) {
	cfg := loadedConfig()
	reqModel := strings.TrimSpace(exec.Model)
	rule, ok := matchingRule(cfg, executionSourceFormat(exec), reqModel)
	if !ok {
		logHostFn(hostCallbackID, "debug", "model-fallback-router: declined executor request", map[string]any{
			"requested_model": reqModel,
			"source_format":   normalizeProtocol(executionSourceFormat(exec)),
			"reason":          "no matching fallback rule",
		})
		return nil, nil, nil, statusError{status: http.StatusBadGateway, message: "no fallback rule matched executor request"}
	}
	policy := fallbackPolicy(cfg, rule)
	primary := resolveModelToken(rule.PrimaryModel, reqModel)
	cooldownKey := fallbackCooldownKey(executionSourceFormat(exec), rule, primary)
	_, primarySkipped := primaryCooldowns.active(cooldownKey)
	plan := buildAttemptPlan(rule, reqModel, primarySkipped)
	attempts := plan.Attempts
	if len(attempts) == 0 {
		return nil, nil, nil, statusError{status: http.StatusBadGateway, message: "fallback rule produced no model attempts"}
	}
	logHostFn(hostCallbackID, "debug", "model-fallback-router: starting fallback chain", map[string]any{
		"requested_model":       reqModel,
		"source_format":         normalizeProtocol(executionSourceFormat(exec)),
		"rule":                  rule.Name,
		"attempts":              append([]string(nil), attempts...),
		"primary_cooldown_skip": plan.PrimarySkipped,
	})

	var lastErr error
	bodyInfo := requestBodyInfo(exec)
	for index, model := range attempts {
		body := requestBodyForModel(bodyInfo.Body, model)
		body, transformMetadata, unwrapAuditedExit, errTransform := applyExecutionTransform(cfg, exec, rule, model, bodyInfo.EntryProtocol, body)
		if errTransform != nil {
			return nil, nil, transformMetadata, errTransform
		}
		resp, errExecute := executeHostModelAttempt(exec, hostCallbackID, model, bodyInfo.EntryProtocol, bodyInfo.ResponseProtocol, body)
		status := responseStatus(resp.StatusCode, errExecute)
		// CPA can preserve an upstream failure body while reporting the host
		// execution as HTTP 200. Treat structured capacity/overload bodies as
		// failures so the fallback chain gets a chance to run.
		if errExecute == nil && successStatus(status) {
			if bodyErr := hostModelBodyError(resp.Body); bodyErr != nil {
				errExecute = bodyErr
				status = statusFromError(bodyErr)
			} else {
				metadata := attemptMetadata(rule, attempts, model, index, plan.PrimarySkipped)
				mergeMetadata(metadata, transformMetadata)
				logHostFn(hostCallbackID, "info", "model-fallback-router: attempt succeeded", map[string]any{
					"rule":                  rule.Name,
					"selected_model":        model,
					"selected_attempt":      index,
					"fallback_used":         plan.PrimarySkipped || index > 0,
					"primary_cooldown_skip": plan.PrimarySkipped,
				})
				payload := resp.Body
				if unwrapAuditedExit {
					unwrapped, exitMetadata, okUnwrap := unwrapAuditedExitResponse(resolveExecutionTransform(cfg, rule), bodyInfo.ResponseProtocol, resp.Body)
					mergeMetadata(metadata, exitMetadata)
					if okUnwrap {
						payload = unwrapped
					}
				}
				return payload, cloneHeader(resp.Headers), metadata, nil
			}
		}
		if errExecute == nil {
			errExecute = hostModelStatusError(model, status, resp.Body)
		}
		lastErr = errExecute
		fallbackAllowed := shouldFallback(status, errExecute, policy)
		if fallbackAllowed && strings.EqualFold(model, plan.Primary) {
			primaryCooldowns.mark(cooldownKey, fallbackCooldownDuration(policy))
		}
		fields := map[string]any{
			"rule":              rule.Name,
			"selected_model":    model,
			"selected_attempt":  index,
			"status":            status,
			"fallback_eligible": fallbackAllowed,
		}
		if fallbackAllowed && index < len(attempts)-1 {
			fields["next_model"] = attempts[index+1]
			logHostFn(hostCallbackID, "info", "model-fallback-router: attempt failed, falling back", fields)
		} else {
			logHostFn(hostCallbackID, "warn", "model-fallback-router: attempt failed, returning upstream error", fields)
		}
		if index == len(attempts)-1 || !fallbackAllowed {
			return nil, nil, nil, errExecute
		}
	}
	if lastErr != nil {
		return nil, nil, nil, lastErr
	}
	return nil, nil, nil, statusError{status: http.StatusBadGateway, message: "fallback execution failed"}
}

func hostModelStatusError(model string, status int, body []byte) error {
	message := fmt.Sprintf("host model %s returned status %d", model, status)
	if summary := hostModelErrorSummary(body); summary != "" {
		message += ": " + summary
	}
	return statusError{status: status, message: message}
}

func hostModelErrorSummary(body []byte) string {
	summary := strings.TrimSpace(string(body))
	if summary == "" {
		return ""
	}
	if len(summary) > 512 {
		summary = summary[:512]
	}
	return summary
}

func hostModelBodyError(body []byte) error {
	var value map[string]any
	if json.Unmarshal(bytes.TrimSpace(body), &value) != nil {
		return nil
	}
	if !isStructuredModelFailure(value) {
		return nil
	}
	message := hostModelErrorSummary(body)
	return statusError{status: http.StatusBadGateway, message: message}
}

func isStructuredModelFailure(value map[string]any) bool {
	if value == nil {
		return false
	}
	if nested, ok := value["error"].(map[string]any); ok {
		return isModelUnavailableError(statusError{message: hostModelErrorSummaryMap(nested)}) ||
			isRateLimitError(statusError{message: hostModelErrorSummaryMap(nested)}) ||
			isAuthUnavailableError(statusError{message: hostModelErrorSummaryMap(nested)})
	}
	return false
}

func hostModelErrorSummaryMap(value map[string]any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func attemptMetadata(rule fallbackRule, attempts []string, selected string, index int, primarySkipped bool) map[string]any {
	metadata := map[string]any{
		"fallback_rule":    rule.Name,
		"attempts":         append([]string(nil), attempts...),
		"selected_model":   selected,
		"fallback_used":    primarySkipped || index > 0,
		"selected_attempt": index,
	}
	if primarySkipped {
		metadata["primary_cooldown_skipped"] = true
	}
	return metadata
}

func mergeMetadata(dst map[string]any, src map[string]any) {
	if dst == nil || src == nil {
		return
	}
	for key, value := range src {
		dst[key] = value
	}
}

func executionSourceFormat(exec pluginapi.ExecutorRequest) string {
	return firstNonEmpty(exec.SourceFormat, exec.Format)
}

func hostProtocol(exec pluginapi.ExecutorRequest) string {
	protocol := normalizeProtocol(executionSourceFormat(exec))
	if protocol == "" {
		return "openai"
	}
	return protocol
}

func responseStatus(status int, err error) int {
	if status > 0 {
		return status
	}
	if errStatus := statusFromError(err); errStatus > 0 {
		return errStatus
	}
	if err == nil {
		return http.StatusOK
	}
	return 0
}
