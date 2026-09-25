package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type streamOrchestrationRunner func(context.Context, pluginapi.ExecutorRequest, string, string) error

type pluginStreamCloser func(string, string)

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return startExecutorStream(req, runExecutionFallbackStream, closePluginStream)
}

func startExecutorStream(req rpcExecutorRequest, runner streamOrchestrationRunner, closeStream pluginStreamCloser) ([]byte, error) {
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	if runner == nil {
		return errorEnvelope("executor_error", "stream orchestration runner is unavailable"), nil
	}
	if closeStream == nil {
		closeStream = func(string, string) {}
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closeStream(streamID, fmt.Sprintf("stream orchestration panic: %v", recovered))
			}
		}()
		errRun := runner(context.Background(), req.ExecutorRequest, req.HostCallbackID, streamID)
		if errRun != nil {
			closeStream(streamID, errRun.Error())
			return
		}
		closeStream(streamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func runExecutionFallbackStream(parent context.Context, exec pluginapi.ExecutorRequest, hostCallbackID, pluginStreamID string) error {
	cfg := loadedConfig()
	reqModel := strings.TrimSpace(exec.Model)
	rule, ok := matchingRule(cfg, executionSourceFormat(exec), reqModel)
	if !ok {
		logHostFn(hostCallbackID, "debug", "model-fallback-router: declined executor stream request", map[string]any{
			"requested_model": reqModel,
			"source_format":   normalizeProtocol(executionSourceFormat(exec)),
			"reason":          "no matching fallback rule",
		})
		return statusError{status: http.StatusBadGateway, message: "no fallback rule matched executor stream request"}
	}
	policy := fallbackPolicy(cfg, rule)
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(policy.MaxElapsedSeconds)*time.Second)
	defer cancel()
	started := time.Now()
	primary := resolveModelToken(rule.PrimaryModel, reqModel)
	cooldownKey := fallbackCooldownKey(executionSourceFormat(exec), rule, primary)
	_, primarySkipped := primaryCooldowns.active(cooldownKey)
	plan := buildAttemptPlan(rule, reqModel, primarySkipped)
	attempts := plan.Attempts
	if len(attempts) == 0 {
		return statusError{status: http.StatusBadGateway, message: "fallback rule produced no stream model attempts"}
	}
	logHostFn(hostCallbackID, "info", "model-fallback-router: starting fallback stream chain", map[string]any{
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
			return err
		}
		body := requestBodyForModel(bodyInfo.Body, model)
		body, _, _, errTransform := applyExecutionTransform(cfg, exec, rule, model, bodyInfo.EntryProtocol, body)
		if errTransform != nil {
			return errTransform
		}
		// Bound each native host call as well as the whole chain. This is
		// essential during CPA shutdown: a provider stream that never returns
		// must not hold the service drain open for the full retry budget.
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, 15*time.Second)
		status, emitted, errForward := forwardHostModelStreamContext(attemptCtx, exec, hostCallbackID, model, bodyInfo.EntryProtocol, bodyInfo.ResponseProtocol, body, pluginStreamID)
		cancelAttempt()
		if errForward == nil && successStatus(responseStatus(status, nil)) {
			logHostFn(hostCallbackID, "info", "model-fallback-router: stream attempt succeeded", map[string]any{
				"rule":                  rule.Name,
				"selected_model":        model,
				"selected_attempt":      index,
				"fallback_used":         plan.PrimarySkipped || index > 0,
				"primary_cooldown_skip": plan.PrimarySkipped,
			})
			return nil
		}
		if errForward == nil {
			errForward = statusError{status: status, message: fmt.Sprintf("host model %s stream returned status %d", model, status)}
		}
		lastErr = errForward
		fallbackAllowed := ctx.Err() == nil && shouldFallback(responseStatus(status, errForward), errForward, policy)
		if fallbackAllowed && strings.EqualFold(model, plan.Primary) {
			primaryCooldowns.mark(cooldownKey, fallbackCooldownDuration(policy))
		}
		fields := map[string]any{
			"rule":              rule.Name,
			"selected_model":    model,
			"selected_attempt":  index,
			"status":            responseStatus(status, errForward),
			"fallback_eligible": fallbackAllowed,
			"client_emitted":    emitted,
		}
		mergeMetadata(fields, retryFields(hostCallbackID, index, len(attempts), started, errForward, fallbackAllowed, emitted))
		fields["delay_before_attempt_ms"] = delay.Milliseconds()
		if fallbackAllowed && !emitted && index < len(attempts)-1 {
			fields["next_model"] = attempts[index+1]
			logHostFn(hostCallbackID, "info", "model-fallback-router: stream attempt failed, falling back", fields)
		} else {
			logHostFn(hostCallbackID, "warn", "model-fallback-router: stream attempt failed, returning upstream error", fields)
		}
		if emitted || index == len(attempts)-1 || !fallbackAllowed {
			return errForward
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return statusError{status: http.StatusBadGateway, message: "fallback stream execution failed"}
}

func forwardHostModelStream(exec pluginapi.ExecutorRequest, hostCallbackID, model, entryProtocol, responseProtocol string, body []byte, pluginStreamID string) (int, bool, error) {
	return forwardHostModelStreamContext(context.Background(), exec, hostCallbackID, model, entryProtocol, responseProtocol, body, pluginStreamID)
}

func forwardHostModelStreamContext(ctx context.Context, exec pluginapi.ExecutorRequest, hostCallbackID, model, entryProtocol, responseProtocol string, body []byte, pluginStreamID string) (int, bool, error) {
	resp, errStart := awaitHost(ctx, func() (pluginapi.HostModelStreamResponse, error) {
		return startHostModelStreamFn(exec, hostCallbackID, model, entryProtocol, responseProtocol, body)
	}, func(resp pluginapi.HostModelStreamResponse) { _ = closeHostModelStreamFn(resp.StreamID) })
	if errStart != nil {
		return responseStatus(0, errStart), false, errStart
	}
	if resp.StatusCode >= 400 {
		_ = closeHostModelStreamFn(resp.StreamID)
		return resp.StatusCode, false, statusError{status: resp.StatusCode, message: fmt.Sprintf("host model %s stream returned status %d", model, resp.StatusCode)}
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return 0, false, fmt.Errorf("host model stream: empty stream_id")
	}
	defer func() { _ = closeHostModelStreamFn(resp.StreamID) }()

	emitted := false
	gate := &streamGate{}
	for {
		chunk, errRead := awaitHost(ctx, func() (pluginapi.HostModelStreamReadResponse, error) { return readHostModelStreamFn(resp.StreamID) }, nil)
		if errRead != nil {
			return responseStatus(0, errRead), emitted, errRead
		}
		if chunk.Error != "" {
			return responseStatus(0, fmt.Errorf("%s", chunk.Error)), emitted, statusError{status: statusFromError(fmt.Errorf("%s", chunk.Error)), message: chunk.Error}
		}
		if len(chunk.Payload) > 0 {
			flush, errFailure, ready := gate.push(chunk.Payload)
			if errFailure != nil {
				return responseStatus(0, errFailure), emitted, errFailure
			}
			if !ready {
				// Do not flush a buffered stream merely because it ended. The
				// buffer may contain only SSE metadata or [DONE], which CPA would
				// treat as an empty upstream response and return 502.
				if len(flush) == 0 {
					if chunk.Done {
						return http.StatusBadGateway, emitted, statusError{status: http.StatusBadGateway, message: "upstream stream closed before first payload"}
					}
					continue
				}
			}
			if len(flush) > 0 {
				emitted = true
			}
			if errEmit := emitPluginStreamChunkFn(pluginStreamID, flush); errEmit != nil {
				return 0, true, errEmit
			}
		}
		if chunk.Done {
			if !emitted && streamPayloadReady(gate.buf.Bytes()) {
				if flush := gate.flush(); len(flush) > 0 {
					emitted = true
					if errEmit := emitPluginStreamChunkFn(pluginStreamID, flush); errEmit != nil {
						return 0, true, errEmit
					}
				}
			}
			if !emitted {
				return http.StatusBadGateway, false, statusError{status: http.StatusBadGateway, message: "upstream stream closed before first payload"}
			}
			return http.StatusOK, emitted, nil
		}
	}
}
