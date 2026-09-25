package main

import (
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"testing"
	"time"
)

func TestStreamCancellationStopsBeforeNextAttempt(t *testing.T) {
	configureFallbackTest(t, 60)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := stubHostStream(t, nil)
	readHostModelStreamFn = func(string) (pluginapi.HostModelStreamReadResponse, error) {
		cancel()
		return pluginapi.HostModelStreamReadResponse{Error: "Selected model is at capacity."}, nil
	}
	if err := runExecutionFallbackStream(ctx, testExecutorRequest(), "callback", "stream"); err == nil {
		t.Fatal("expected cancellation failure")
	}
	if len(state.calls) != 1 {
		t.Fatalf("started %d attempts after cancellation", len(state.calls))
	}
}

func TestStreamDeadlineClosesBlockedRead(t *testing.T) {
	configureFallbackTest(t, 60)
	state := stubHostStream(t, nil)
	released := make(chan struct{})
	exited := make(chan struct{})
	readHostModelStreamFn = func(string) (pluginapi.HostModelStreamReadResponse, error) {
		defer close(exited)
		<-released
		return pluginapi.HostModelStreamReadResponse{Done: true}, nil
	}
	closeHostModelStreamFn = func(string) error { close(released); return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := runExecutionFallbackStream(ctx, testExecutorRequest(), "callback", "stream")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	<-exited
	if len(state.calls) != 1 {
		t.Fatalf("deadline triggered extra attempts: %v", state.calls)
	}
}

func TestBackoffBoundsAndCancellation(t *testing.T) {
	p := fallbackSettings{RetryBaseMS: 500, RetryMaxMS: 8000}
	for index, want := range []time.Duration{0, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second} {
		delay := retryDelay(p, index)
		if delay < want/2 || delay > want {
			t.Fatalf("attempt %d delay %v outside [%v,%v]", index, delay, want/2, want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(waitRetry(ctx, time.Hour), context.Canceled) {
		t.Fatal("cancelled backoff did not stop")
	}
}

func TestTenSameModelRetriesRecoverWithoutLeakingFailures(t *testing.T) {
	configureFallbackTest(t, 0)
	cfg := loadedConfig()
	cfg.Rules[0].FallbackModels = make([]string, 10)
	for i := range cfg.Rules[0].FallbackModels {
		cfg.Rules[0].FallbackModels[i] = "$requested"
	}
	cfg.Fallback.RetryBaseMS = 0
	currentConfig.Store(cfg)
	chunks := make([]pluginapi.HostModelStreamReadResponse, 10)
	for i := range chunks {
		chunks[i] = pluginapi.HostModelStreamReadResponse{Payload: []byte(capacityFailureFrame)}
	}
	chunks = append(chunks, pluginapi.HostModelStreamReadResponse{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered\"}\n\n"), Done: true})
	state := stubHostStream(t, map[string][]pluginapi.HostModelStreamReadResponse{"claude-sonnet-4-5": chunks})
	if err := runExecutionFallbackStream(context.Background(), testExecutorRequest(), "callback", "stream"); err != nil {
		t.Fatal(err)
	}
	if len(state.calls) != 11 || len(state.emitted) != 1 {
		t.Fatalf("calls=%d emitted=%d", len(state.calls), len(state.emitted))
	}
}
