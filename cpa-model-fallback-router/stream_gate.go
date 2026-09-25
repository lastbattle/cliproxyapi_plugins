package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// streamGate delays the first client-visible bytes until the upstream has
// produced a non-error stream event. This leaves a retry window for providers
// that report overload as an SSE response.failed/error event after returning
// HTTP 200. CPA's observed terminal shapes include:
//
//	event: response.failed
//	data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model."}}}
//
// and:
//
//	event: error
//	data: {"type":"error","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}
type streamGate struct {
	buf       bytes.Buffer
	committed bool
}

func (g *streamGate) flush() []byte {
	if g.buf.Len() == 0 {
		return nil
	}
	g.committed = true
	return append([]byte(nil), g.buf.Bytes()...)
}

const maxStreamPrelude = 256 * 1024

func (g *streamGate) push(payload []byte) (flush []byte, failure error, ready bool) {
	if g.committed {
		return payload, nil, true
	}
	_, _ = g.buf.Write(payload)
	if failure = streamPayloadFailure(g.buf.Bytes()); failure != nil {
		return nil, failure, false
	}
	if streamPayloadReady(g.buf.Bytes()) || g.buf.Len() >= maxStreamPrelude {
		g.committed = true
		return append([]byte(nil), g.buf.Bytes()...), nil, true
	}
	return nil, nil, false
}

func streamPayloadFailure(payload []byte) error {
	if value, ok := rawJSONObject(payload); ok && streamJSONFailure(value) {
		return statusError{status: streamStatus(value), message: streamErrorMessage(value)}
	}
	for _, record := range sseRecords(payload) {
		if streamJSONFailure(record) {
			return statusError{status: streamStatus(record), message: streamErrorMessage(record)}
		}
	}
	return nil
}

func rawJSONObject(payload []byte) (map[string]any, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var value map[string]any
	if json.Unmarshal(trimmed, &value) != nil {
		return nil, false
	}
	return value, true
}

func streamPayloadReady(payload []byte) bool {
	records := sseRecords(payload)
	if len(records) == 0 && bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
		var value map[string]any
		if json.Unmarshal(bytes.TrimSpace(payload), &value) == nil {
			return !streamJSONFailure(value)
		}
	}
	for _, record := range records {
		if streamJSONFailure(record) {
			return false
		}
		kind, _ := record["type"].(string)
		kind = strings.ToLower(kind)
		// Responses metadata is not generated content. Hold empty item/part
		// announcements until a delta or terminal record makes forwarding safe.
		if strings.HasPrefix(kind, "response.") &&
			(kind == "response.output_item.added" || kind == "response.content_part.added" ||
				kind == "response.reasoning_summary_part.added" || kind == "response.queued") {
			item, _ := record["item"].(map[string]any)
			if item["type"] != "function_call" {
				continue
			}
		}
		if strings.HasSuffix(kind, ".delta") || strings.HasSuffix(kind, ".completed") ||
			strings.HasSuffix(kind, ".stop") || kind == "content_block_start" ||
			kind == "message_delta" || kind == "message_stop" {
			return true
		}
		// Unknown complete JSON stream records are safe to forward. Keeping only
		// known metadata events buffered prevents breaking other protocols.
		if kind != "response.created" && kind != "response.in_progress" &&
			kind != "message_start" && kind != "content_block_start" && kind != "ping" {
			return true
		}
	}
	return false
}

func sseRecords(payload []byte) []map[string]any {
	parts := bytes.Split(bytes.ReplaceAll(payload, []byte("\r\n"), []byte("\n")), []byte("\n\n"))
	result := make([]map[string]any, 0, len(parts))
	// Only inspect complete SSE frames; network chunks may split JSON or lines.
	for _, part := range parts[:len(parts)-1] {
		for _, line := range bytes.Split(part, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
				continue
			}
			var value map[string]any
			if json.Unmarshal(data, &value) == nil {
				result = append(result, value)
			}
		}
	}
	return result
}

func streamJSONFailure(value map[string]any) bool {
	if value == nil {
		return false
	}
	for _, key := range []string{"type", "event_type", "status", "code"} {
		if text, ok := value[key].(string); ok {
			lower := strings.ToLower(text)
			if lower == "error" || lower == "response.failed" || lower == "response.error" || lower == "failed" ||
				strings.Contains(lower, "overload") || strings.Contains(lower, "capacity") {
				return true
			}
		}
	}
	if nested, ok := value["error"].(map[string]any); ok {
		return strings.TrimSpace(streamErrorMessage(nested)) != ""
	}
	if response, ok := value["response"].(map[string]any); ok {
		if status, _ := response["status"].(string); strings.EqualFold(status, "failed") {
			return true
		}
		if nested, ok := response["error"].(map[string]any); ok {
			return strings.TrimSpace(streamErrorMessage(nested)) != ""
		}
	}
	return false
}

func streamErrorMessage(value map[string]any) string {
	if value == nil {
		return "streamed upstream model failure"
	}
	for _, key := range []string{"error", "response"} {
		if nested, ok := value[key].(map[string]any); ok {
			return streamErrorMessage(nested)
		}
	}
	// Preserve code/type alongside message, without serializing response content.
	if message, ok := value["message"].(string); ok {
		return fmt.Sprintf("%v %v: %s", value["type"], value["code"], message)
	}
	for _, key := range []string{"message", "detail", "error"} {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func streamStatus(value map[string]any) int {
	for _, key := range []string{"status_code", "status", "http_status"} {
		switch number := value[key].(type) {
		case float64:
			if int(number) >= 400 {
				return int(number)
			}
		case string:
			var status int
			if _, err := fmt.Sscan(number, &status); err == nil && status >= 400 {
				return status
			}
		}
	}
	for _, key := range []string{"error", "response"} {
		if nested, ok := value[key].(map[string]any); ok {
			return streamStatus(nested)
		}
	}
	return http.StatusBadGateway
}
