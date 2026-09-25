package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const capacityFailureFrame = "event: response.failed\n" +
	`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model."}}}` +
	"\n\n"

func TestStreamPayloadFailureDetectsCapacityFrame(t *testing.T) {
	errFailure := streamPayloadFailure([]byte(capacityFailureFrame))
	if errFailure == nil {
		t.Fatal("streamPayloadFailure() = nil, want capacity failure")
	}
	if got := statusFromError(errFailure); got != http.StatusBadGateway {
		t.Fatalf("statusFromError() = %d, want %d", got, http.StatusBadGateway)
	}
	if !shouldFallback(http.StatusBadGateway, errFailure, fallbackSettings{
		Enabled:          true,
		FallbackOnStatus: []int{429, 500, 502, 503, 504},
	}) {
		t.Fatal("shouldFallback() = false, want retry for streamed capacity failure")
	}
}

func TestStreamPayloadFailureDetectsRawJSONFailure(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"message":"upstream exploded"}}}`)
	if errFailure := streamPayloadFailure(payload); errFailure == nil {
		t.Fatal("streamPayloadFailure() = nil, want structured failure")
	}
}

func TestStreamPayloadFailureIgnoresPartialJSON(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"status":"fa`)
	if errFailure := streamPayloadFailure(payload); errFailure != nil {
		t.Fatalf("streamPayloadFailure() = %v, want nil for a partial record", errFailure)
	}
}

func TestStreamGateWithholdsFailureUntilRecordCompletes(t *testing.T) {
	gate := &streamGate{}
	if flush, errFailure, ready := gate.push([]byte("data: {\"type\":\"response.created\"}\n\n")); ready || errFailure != nil || len(flush) != 0 {
		t.Fatalf("prelude push = (%q, %v, %t), want buffered", flush, errFailure, ready)
	}
	split := []byte(capacityFailureFrame)
	half := len(split) / 2
	if flush, errFailure, ready := gate.push(split[:half]); errFailure != nil || ready || len(flush) != 0 {
		t.Fatalf("partial failure push = (%q, %v, %t), want buffered", flush, errFailure, ready)
	}
	flush, errFailure, ready := gate.push(split[half:])
	if errFailure == nil {
		t.Fatal("push() failure = nil, want capacity failure once frame completes")
	}
	if ready {
		t.Fatal("push() ready = true, want false on failure")
	}
	if len(flush) != 0 {
		t.Fatalf("push() flush = %q, want nothing emitted before retry", flush)
	}
}

func TestStreamGateFlushesBufferedPreludeOnFirstDelta(t *testing.T) {
	gate := &streamGate{}
	prelude := "data: {\"type\":\"response.created\"}\n\n"
	if _, errFailure, ready := gate.push([]byte(prelude)); errFailure != nil || ready {
		t.Fatalf("prelude push = (%v, %t), want buffered", errFailure, ready)
	}
	delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	flush, errFailure, ready := gate.push([]byte(delta))
	if errFailure != nil {
		t.Fatalf("push() failure = %v, want nil", errFailure)
	}
	if !ready {
		t.Fatal("push() ready = false, want true once content arrives")
	}
	if string(flush) != prelude+delta {
		t.Fatalf("flush = %q, want exact upstream bytes %q", flush, prelude+delta)
	}
}

func TestStreamGatePassesThroughAfterCommit(t *testing.T) {
	gate := &streamGate{}
	if _, _, ready := gate.push([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n")); !ready {
		t.Fatal("push() ready = false, want true on delta")
	}
	payload := []byte("data: {\"type\":\"response.failed\"}\n\n")
	flush, errFailure, ready := gate.push(payload)
	if errFailure != nil || !ready {
		t.Fatalf("post-commit push = (%v, %t), want pass-through", errFailure, ready)
	}
	if string(flush) != string(payload) {
		t.Fatalf("flush = %q, want %q", flush, payload)
	}
}

type stubbedHostStream struct {
	calls   []string
	emitted [][]byte
	queues  map[string][]pluginapi.HostModelStreamReadResponse
}

func stubHostStream(t *testing.T, queues map[string][]pluginapi.HostModelStreamReadResponse) *stubbedHostStream {
	t.Helper()
	originalStart := startHostModelStreamFn
	originalRead := readHostModelStreamFn
	originalEmit := emitPluginStreamChunkFn
	originalClose := closeHostModelStreamFn
	state := &stubbedHostStream{queues: map[string][]pluginapi.HostModelStreamReadResponse{}}
	for model, chunks := range queues {
		state.queues[model] = append([]pluginapi.HostModelStreamReadResponse(nil), chunks...)
	}
	startHostModelStreamFn = func(_ pluginapi.ExecutorRequest, _ string, model, _, _ string, _ []byte) (pluginapi.HostModelStreamResponse, error) {
		state.calls = append(state.calls, model)
		return pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "stream-" + model}, nil
	}
	readHostModelStreamFn = func(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
		model := strings.TrimPrefix(streamID, "stream-")
		queue := state.queues[model]
		if len(queue) == 0 {
			return pluginapi.HostModelStreamReadResponse{Done: true}, nil
		}
		next := queue[0]
		state.queues[model] = queue[1:]
		return next, nil
	}
	emitPluginStreamChunkFn = func(_ string, payload []byte) error {
		state.emitted = append(state.emitted, append([]byte(nil), payload...))
		return nil
	}
	closeHostModelStreamFn = func(string) error { return nil }
	t.Cleanup(func() {
		startHostModelStreamFn = originalStart
		readHostModelStreamFn = originalRead
		emitPluginStreamChunkFn = originalEmit
		closeHostModelStreamFn = originalClose
	})
	return state
}

func TestRunExecutionFallbackStreamRetriesStreamedCapacityFailure(t *testing.T) {
	configureFallbackTest(t, 60)
	fallbackPrelude := "data: {\"type\":\"response.created\"}\n\n"
	fallbackDelta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	state := stubHostStream(t, map[string][]pluginapi.HostModelStreamReadResponse{
		"claude-sonnet-4-5": {
			{Payload: []byte("data: {\"type\":\"response.created\"}\n\n")},
			{Payload: []byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"content\":[]}}\n\n")},
			{Payload: []byte(capacityFailureFrame)},
		},
		"gpt-5.4": {
			{Payload: []byte(fallbackPrelude)},
			{Payload: []byte(fallbackDelta)},
			{Done: true},
		},
	})

	errRun := runExecutionFallbackStream(nil, testExecutorRequest(), "callback-1", "plugin-stream-1")
	if errRun != nil {
		t.Fatalf("runExecutionFallbackStream() error = %v", errRun)
	}
	if len(state.calls) != 2 || state.calls[0] != "claude-sonnet-4-5" || state.calls[1] != "gpt-5.4" {
		t.Fatalf("host calls = %#v, want primary then fallback", state.calls)
	}
	if len(state.emitted) != 1 {
		t.Fatalf("emitted chunks = %d, want a single buffered forward", len(state.emitted))
	}
	if got, want := string(state.emitted[0]), fallbackPrelude+fallbackDelta; got != want {
		t.Fatalf("emitted = %q, want exact fallback upstream bytes %q", got, want)
	}
	if strings.Contains(string(state.emitted[0]), "response.failed") {
		t.Fatal("failed primary attempt leaked into the client stream")
	}
}

func TestStreamGatePreservesQuotedCapacityText(t *testing.T) {
	g := &streamGate{}
	payload := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"Selected model is at capacity. Please try a different model.\"}\n\n")
	got, err, ready := g.push(payload)
	if err != nil || !ready || string(got) != string(payload) {
		t.Fatalf("ordinary content changed: %q, %v, %v", got, err, ready)
	}
}

func TestRunExecutionFallbackStreamRetriesChunkErrorMessage(t *testing.T) {
	configureFallbackTest(t, 60)
	state := stubHostStream(t, map[string][]pluginapi.HostModelStreamReadResponse{
		"claude-sonnet-4-5": {
			{Error: "Selected model is at capacity. Please try a different model.", Done: true},
		},
		"gpt-5.4": {
			{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")},
			{Done: true},
		},
	})

	errRun := runExecutionFallbackStream(nil, testExecutorRequest(), "callback-1", "plugin-stream-1")
	if errRun != nil {
		t.Fatalf("runExecutionFallbackStream() error = %v", errRun)
	}
	if len(state.calls) != 2 || state.calls[1] != "gpt-5.4" {
		t.Fatalf("host calls = %#v, want retry on capacity message", state.calls)
	}
	if len(state.emitted) != 1 || !strings.Contains(string(state.emitted[0]), "\"delta\":\"ok\"") {
		t.Fatalf("emitted = %q, want fallback delta", state.emitted)
	}
}

func TestRunExecutionFallbackStreamStopsAfterFirstEmittedChunk(t *testing.T) {
	configureFallbackTest(t, 60)
	state := stubHostStream(t, map[string][]pluginapi.HostModelStreamReadResponse{
		"claude-sonnet-4-5": {
			{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")},
			{Error: "stream died mid-flight", Done: true},
		},
		"gpt-5.4": {
			{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n")},
			{Done: true},
		},
	})

	errRun := runExecutionFallbackStream(nil, testExecutorRequest(), "callback-1", "plugin-stream-1")
	if errRun == nil {
		t.Fatal("runExecutionFallbackStream() = nil, want the mid-stream error surfaced")
	}
	if len(state.calls) != 1 || state.calls[0] != "claude-sonnet-4-5" {
		t.Fatalf("host calls = %#v, want no retry once bytes reached the client", state.calls)
	}
}

func TestRunExecutionFallbackStreamRetriesEmptyCompletion(t *testing.T) {
	configureSameModelStreamRetryTest(t)
	state := stubHostStream(t, map[string][]pluginapi.HostModelStreamReadResponse{
		"gpt-5.6-sol": {
			{Done: true},
			{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered\"}\n\n")},
			{Done: true},
		},
	})
	errRun := runExecutionFallbackStream(context.Background(), pluginapi.ExecutorRequest{Model: "gpt-5.6-sol", SourceFormat: "openai-response", OriginalRequest: []byte(`{"model":"gpt-5.6-sol","input":[]}`)}, "callback-1", "plugin-stream-1")
	if errRun != nil {
		t.Fatalf("runExecutionFallbackStream() error = %v", errRun)
	}
	if len(state.calls) != 2 || len(state.emitted) != 1 || !strings.Contains(string(state.emitted[0]), "recovered") {
		t.Fatalf("calls=%#v emitted=%q, want retry then payload", state.calls, state.emitted)
	}
}

func TestRunExecutionFallbackStreamRetriesSameModelWhenConfigured(t *testing.T) {
	configureSameModelStreamRetryTest(t)
	state := stubHostStream(t, map[string][]pluginapi.HostModelStreamReadResponse{
		"gpt-5.6-sol": {
			{Payload: []byte("data: {\"type\":\"response.created\"}\n\n")},
			{Payload: []byte(capacityFailureFrame)},
			{Payload: []byte("data: {\"type\":\"response.created\"}\n\n")},
			{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered\"}\n\n")},
			{Done: true},
		},
	})
	exec := pluginapi.ExecutorRequest{
		Model:           "gpt-5.6-sol",
		SourceFormat:    "openai-response",
		OriginalRequest: []byte(`{"model":"gpt-5.6-sol","input":[]}`),
	}

	errRun := runExecutionFallbackStream(nil, exec, "callback-1", "plugin-stream-1")
	if errRun != nil {
		t.Fatalf("runExecutionFallbackStream() error = %v", errRun)
	}
	if len(state.calls) != 2 || state.calls[0] != "gpt-5.6-sol" || state.calls[1] != "gpt-5.6-sol" {
		t.Fatalf("host calls = %#v, want two same-model attempts", state.calls)
	}
	if len(state.emitted) != 1 || !strings.Contains(string(state.emitted[0]), "recovered") {
		t.Fatalf("emitted = %q, want the recovered delta only", state.emitted)
	}
	if strings.Contains(string(state.emitted[0]), "response.failed") {
		t.Fatal("failed attempt leaked into the client stream")
	}
}

func configureSameModelStreamRetryTest(t *testing.T) {
	t.Helper()
	originalCooldowns := primaryCooldowns
	cfg, err := decodeConfig([]byte(`enabled: true
rules:
  - name: transient_same_model_retry
    models:
      - "*"
    primary_model: "$requested"
    fallback_models:
      - "$requested"
      - "$requested"
    fallback_on_status:
      - 429
      - 500
      - 502
      - 503
      - 504
fallback:
  cooldown_seconds: 3
`))
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	currentConfig.Store(cfg)
	primaryCooldowns = newPrimaryCooldownStore(func() time.Time {
		return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	})
	t.Cleanup(func() {
		primaryCooldowns = originalCooldowns
		currentConfig.Store(defaultPluginConfig())
	})
}
