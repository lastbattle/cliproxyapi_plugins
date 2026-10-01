package main

import (
	"errors"
	"net/http"
	"testing"
)

func TestShouldFallbackStatusPolicy(t *testing.T) {
	settings := fallbackSettings{
		Enabled:            true,
		FallbackOnStatus:   []int{429, 503},
		NoFallbackOnStatus: []int{400, 404},
	}
	if !shouldFallback(429, nil, settings) {
		t.Fatal("shouldFallback(429) = false, want true")
	}
	if shouldFallback(400, nil, settings) {
		t.Fatal("shouldFallback(400) = true, want false")
	}
	if !shouldFallback(400, errors.New("prompt is too long: 1035602 tokens > 1000000 maximum"), settings) {
		t.Fatal("shouldFallback(context window 400) = false, want true")
	}
	if !shouldFallback(400, errors.New("context_length_exceeded: maximum context length reached"), settings) {
		t.Fatal("shouldFallback(context_length_exceeded 400) = false, want true")
	}
	if !shouldFallback(0, errors.New("context_length_exceeded: maximum context length reached"), settings) {
		t.Fatal("shouldFallback(context_length_exceeded without preserved status) = false, want true")
	}
	if shouldFallback(400, errors.New("invalid request: missing required field"), settings) {
		t.Fatal("shouldFallback(generic 400) = true, want false")
	}
	if !shouldFallback(0, errors.New("connection reset by peer"), settings) {
		t.Fatal("shouldFallback(network error) = false, want true")
	}
	if !shouldFallback(502, errors.New("An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists."), settings) {
		t.Fatal("shouldFallback(upstream server error) = false, want true")
	}
	if !shouldFallback(http.StatusBadGateway, errors.New("context deadline exceeded"), settings) {
		t.Fatal("shouldFallback(502, context deadline exceeded) = false, want true")
	}
	if !shouldFallback(0, errors.New("This request would exceed your account's rate limit. Please try again later."), settings) {
		t.Fatal("shouldFallback(rate limit text) = false, want true")
	}
	if !shouldFallback(0, errors.New("auth_unavailable: no auth available"), settings) {
		t.Fatal("shouldFallback(auth unavailable text) = false, want true")
	}
	if !shouldFallback(0, errors.New("auth_not_found: no auth available"), settings) {
		t.Fatal("shouldFallback(auth not found text) = false, want true")
	}
	if !shouldFallback(0, errors.New("model_cooldown: model is cooling down"), settings) {
		t.Fatal("shouldFallback(model cooldown text) = false, want true")
	}
	if !shouldFallback(0, errors.New("account disabled by operator"), settings) {
		t.Fatal("shouldFallback(disabled account text) = false, want true")
	}
	if !shouldFallback(0, errors.New("host_call_failed: unknown provider for model claude-haiku-4-5-20251001"), settings) {
		t.Fatal("shouldFallback(unknown provider text) = false, want true")
	}
	settings.Enabled = false
	if shouldFallback(429, nil, settings) {
		t.Fatal("disabled shouldFallback(429) = true, want false")
	}
}

func TestStatusFromError(t *testing.T) {
	if got := statusFromError(statusError{status: 503}); got != 503 {
		t.Fatalf("statusFromError(statusError) = %d, want 503", got)
	}
	if got := statusFromError(errors.New("model execution failed with status 429")); got != 429 {
		t.Fatalf("statusFromError(message) = %d, want 429", got)
	}
}

// CPA can report an auth-unavailable/overload failure with a numeric status the
// operator did not enumerate. The message text still identifies a retryable
// condition, so the classifier must consult it rather than treating it as terminal.
func TestShouldFallbackRecognizesCodexAuthUnavailableOnUnlistedStatus(t *testing.T) {
	settings := fallbackSettings{
		Enabled:            true,
		FallbackOnStatus:   []int{401, 403, 408, 409, 429, 500, 502, 503, 504},
		NoFallbackOnStatus: []int{400, 404, 422},
	}
	shape := errors.New("unexpected status 501 Service Unavailable: auth_unavailable: no auth available (providers=codex, model=gpt-5.6-sol; last upstream error: server_is_overloaded: Our servers are currently overloaded. Please try again later.)")
	if !shouldFallback(501, shape, settings) {
		t.Fatal("shouldFallback(501, codex auth_unavailable shape) = false, want true")
	}
	if !shouldFallback(0, shape, settings) {
		t.Fatal("shouldFallback(0, codex auth_unavailable shape) = false, want true")
	}
	if !shouldFallback(0, errors.New("server_is_overloaded"), settings) {
		t.Fatal("shouldFallback(0, server_is_overloaded) = false, want true")
	}
	// An operator-configured terminal status still wins over matching text.
	if shouldFallback(http.StatusNotFound, shape, settings) {
		t.Fatal("shouldFallback(404, codex auth_unavailable shape) = true, want false")
	}
	// An unlisted status with no recognizable text stays terminal.
	if shouldFallback(501, errors.New("some unexpected upstream fault"), settings) {
		t.Fatal("shouldFallback(501, generic text) = true, want false")
	}
}
