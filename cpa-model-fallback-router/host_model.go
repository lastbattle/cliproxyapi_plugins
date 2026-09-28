package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type hostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

// logHost emits a best-effort structured log line through the CPA host so routing
// and fallback decisions are visible in the host log stream. A logging failure is
// never allowed to affect request handling.
func logHost(hostCallbackID, level, message string, fields map[string]any) {
	if strings.TrimSpace(message) == "" {
		return
	}
	// CPA's text log can omit Fields: main.log on 2026-09-28 at 07:21:33
	// recorded only "stream attempt failed, returning upstream error" for
	// request 00000205, hiding client_emitted and stop_reason. Include the
	// router's diagnostic fields in the message too; never include raw bodies.
	if len(fields) > 0 {
		if encoded, err := json.Marshal(fields); err == nil {
			message += " " + string(encoded)
		}
	}
	_, _ = callHost(pluginabi.MethodHostLog, hostLogRequest{
		HostCallbackID: hostCallbackID,
		Level:          level,
		Message:        message,
		Fields:         fields,
	})
}

func executeHostModel(exec pluginapi.ExecutorRequest, hostCallbackID, model, entryProtocol, responseProtocol string, body []byte) (pluginapi.HostModelExecutionResponse, error) {
	result, errCall := callHost(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
		HostModelExecutionRequest: hostModelExecutionPayload(exec, model, entryProtocol, responseProtocol, false, body),
		HostCallbackID:            hostCallbackID,
	})
	if errCall != nil {
		return pluginapi.HostModelExecutionResponse{}, errCall
	}
	var resp pluginapi.HostModelExecutionResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return pluginapi.HostModelExecutionResponse{}, fmt.Errorf("decode host.model.execute result: %w", errUnmarshal)
	}
	return resp, nil
}

func startHostModelStream(exec pluginapi.ExecutorRequest, hostCallbackID, model, entryProtocol, responseProtocol string, body []byte) (pluginapi.HostModelStreamResponse, error) {
	result, errCall := callHost(pluginabi.MethodHostModelExecuteStream, hostModelExecutionRequest{
		HostModelExecutionRequest: hostModelExecutionPayload(exec, model, entryProtocol, responseProtocol, true, body),
		HostCallbackID:            hostCallbackID,
	})
	if errCall != nil {
		return pluginapi.HostModelStreamResponse{}, errCall
	}
	var resp pluginapi.HostModelStreamResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return pluginapi.HostModelStreamResponse{}, fmt.Errorf("decode host.model.execute_stream result: %w", errUnmarshal)
	}
	return resp, nil
}

func hostModelExecutionPayload(exec pluginapi.ExecutorRequest, model, entryProtocol, responseProtocol string, stream bool, body []byte) pluginapi.HostModelExecutionRequest {
	entryProtocol = normalizeProtocol(entryProtocol)
	responseProtocol = normalizeProtocol(responseProtocol)
	if entryProtocol == "" {
		entryProtocol = hostProtocol(exec)
	}
	if responseProtocol == "" {
		responseProtocol = entryProtocol
	}
	return pluginapi.HostModelExecutionRequest{
		EntryProtocol: entryProtocol,
		ExitProtocol:  responseProtocol,
		Model:         model,
		Stream:        stream,
		Body:          body,
		Headers:       cloneHeader(exec.Headers),
		Query:         cloneValues(exec.Query),
		Alt:           exec.Alt,
	}
}

func readHostModelStream(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
	result, errCall := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: streamID})
	if errCall != nil {
		return pluginapi.HostModelStreamReadResponse{}, errCall
	}
	var chunk pluginapi.HostModelStreamReadResponse
	if errUnmarshal := json.Unmarshal(result, &chunk); errUnmarshal != nil {
		return pluginapi.HostModelStreamReadResponse{}, fmt.Errorf("decode host.model.stream_read result: %w", errUnmarshal)
	}
	return chunk, nil
}

func closeHostModelStream(streamID string) error {
	if strings.TrimSpace(streamID) == "" {
		return nil
	}
	_, errCall := callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
	return errCall
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return errCall
}

func closePluginStream(streamID, errMsg string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errMsg),
	})
}

// Host model stream calls are indirected so the upstream-first buffering and
// retry orchestration can be exercised without a live plugin host.
var (
	startHostModelStreamFn  = startHostModelStream
	readHostModelStreamFn   = readHostModelStream
	emitPluginStreamChunkFn = emitPluginStreamChunk
	closeHostModelStreamFn  = closeHostModelStream
	logHostFn               = logHost
)
