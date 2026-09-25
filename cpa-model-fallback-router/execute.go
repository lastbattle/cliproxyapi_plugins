package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

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
		// CPA derives the host-local ID from the installed filename. A library
		// installed as capacity-retry.dylib must target capacity-retry, not its
		// metadata name, or CPA bypasses this executor as unavailable.
		Target: firstNonEmpty(req.PluginID, pluginIdentifier),
		Reason: pluginIdentifier + ":matched",
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
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(policy.MaxElapsedSeconds)*time.Second)
	defer cancel()
	primary := resolveModelToken(rule.PrimaryModel, reqModel)
	cooldownKey := fallbackCooldownKey(executionSourceFormat(exec), rule, primary)
	_, primarySkipped := primaryCooldowns.active(cooldownKey)
	plan := buildAttemptPlan(rule, reqModel, primarySkipped)
	attempts := plan.Attempts
	if len(attempts) == 0 {
		return nil, nil, nil, statusError{status: http.StatusBadGateway, message: "fallback rule produced no model attempts"}
	}
	logHostFn(hostCallbackID, "info", "model-fallback-router: starting fallback chain", map[string]any{
		"request_id":            hostCallbackID,
		"requested_model":       reqModel,
		"source_format":         normalizeProtocol(executionSourceFormat(exec)),
		"rule":                  rule.Name,
		"attempts":              append([]string(nil), attempts...),
		"primary_cooldown_skip": plan.PrimarySkipped,
	})

	var lastErr error
	bodyInfo := requestBodyInfo(exec)
	for index, model := range attempts {
		delay := retryDelay(policy, index)
		if err := waitRetry(ctx, delay); err != nil {
			logHostFn(hostCallbackID, "warn", "model-fallback-router: retry wait stopped", retryFields(hostCallbackID, index, len(attempts), started, err, false, false))
			return nil, nil, nil, err
		}
		body := requestBodyForModel(bodyInfo.Body, model)
		body, transformMetadata, unwrapAuditedExit, errTransform := applyExecutionTransform(cfg, exec, rule, model, bodyInfo.EntryProtocol, body)
		if errTransform != nil {
			return nil, nil, transformMetadata, errTransform
		}
		resp, errExecute := awaitHost(ctx, func() (pluginapi.HostModelExecutionResponse, error) {
			return executeHostModelAttempt(exec, hostCallbackID, model, bodyInfo.EntryProtocol, bodyInfo.ResponseProtocol, body)
		}, nil)
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
		fallbackAllowed := ctx.Err() == nil && shouldFallback(status, errExecute, policy)
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
		mergeMetadata(fields, retryFields(hostCallbackID, index, len(attempts), started, errExecute, fallbackAllowed, false))
		fields["delay_before_attempt_ms"] = delay.Milliseconds()
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
	if !streamJSONFailure(value) {
		return nil
	}
	return statusError{status: streamStatus(value), message: streamErrorMessage(value)}
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
