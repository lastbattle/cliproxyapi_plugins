package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"time"
)

func isCancellation(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return errors.Is(err, context.Canceled) || strings.Contains(text, "context canceled") || strings.Contains(text, "context cancelled") || strings.Contains(text, "is not open")
}

// Equal jitter: a nonzero floor prevents immediate retry bursts during overload.
func retryDelay(policy fallbackSettings, index int) time.Duration {
	if index <= 0 {
		return 0
	}
	delay := time.Duration(policy.RetryBaseMS) * time.Millisecond
	cap := time.Duration(policy.RetryMaxMS) * time.Millisecond
	for n := 1; n < index && delay < cap; n++ {
		delay *= 2
	}
	if delay > cap {
		delay = cap
	}
	if delay <= 0 {
		return 0
	}
	return delay/2 + time.Duration(rand.Int64N(int64(delay-delay/2)+1))
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// Native callbacks cannot accept a Go context across the ABI. Bound waiting
// locally; a late stream-open result must be closed by its caller.
func awaitHost[T any](ctx context.Context, call func() (T, error), late func(T)) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result)
	go func() {
		value, err := call()
		select {
		case done <- result{value, err}:
		case <-ctx.Done():
			if late != nil {
				late(value)
			}
		}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func errorCategory(err error) string {
	switch {
	case err == nil:
		return "none"
	case isCancellation(err):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case isModelUnavailableError(err):
		return "model_unavailable"
	case isAuthUnavailableError(err):
		return "auth_unavailable"
	case isRateLimitError(err):
		return "rate_limit"
	case isContextWindowError(err):
		return "context_length"
	case isNetworkError(err):
		return "transport"
	default:
		return "upstream_error"
	}
}

func retryFields(callback string, index, total int, started time.Time, err error, eligible, emitted bool) map[string]any {
	reason := "retry"
	if emitted {
		reason = "content_emitted"
	} else if !eligible {
		reason = "not_retryable"
	} else if index+1 >= total {
		reason = "attempts_exhausted"
	}
	code := "unknown"
	if err != nil {
		for _, known := range []string{"server_is_overloaded", "model_at_capacity", "auth_unavailable", "model_not_found", "context_length_exceeded", "rate_limit_exceeded"} {
			if strings.Contains(strings.ToLower(err.Error()), known) {
				code = known
				break
			}
		}
	}
	return map[string]any{"request_id": callback, "attempt": index + 1, "max_attempts": total, "elapsed_ms": time.Since(started).Milliseconds(), "error_category": errorCategory(err), "error_code": code, "stop_reason": reason}
}
