package auth

import (
	"fmt"
	"testing"
	"time"
)

type stubRetryAfterErr struct {
	msg        string
	status     int
	retryAfter *time.Duration
}

func (e stubRetryAfterErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return "stub retry-after error"
}

func (e stubRetryAfterErr) StatusCode() int { return e.status }

func (e stubRetryAfterErr) RetryAfter() *time.Duration { return e.retryAfter }

type wrapErr struct {
	cause error
}

func (e *wrapErr) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *wrapErr) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func TestRetryAfterFromErrorUnwrapsWrappedStatus(t *testing.T) {
	d := 24 * time.Hour
	inner := stubRetryAfterErr{
		msg:        `{"code":"subscription:free-usage-exhausted"}`,
		status:     429,
		retryAfter: &d,
	}
	// Mirrors streamBootstrapError: outer wrapper only implements Unwrap.
	wrapped := &wrapErr{cause: inner}

	got := retryAfterFromError(wrapped)
	if got == nil {
		t.Fatal("expected RetryAfter from wrapped free-usage error")
	}
	if *got != 24*time.Hour {
		t.Fatalf("RetryAfter = %v, want 24h", *got)
	}

	// Direct (unwrapped) path still works.
	gotDirect := retryAfterFromError(inner)
	if gotDirect == nil || *gotDirect != 24*time.Hour {
		t.Fatalf("direct RetryAfter = %v, want 24h", gotDirect)
	}

	// Unrelated wrapped error stays nil.
	if gotNil := retryAfterFromError(fmt.Errorf("wrap: %w", fmt.Errorf("nope"))); gotNil != nil {
		t.Fatalf("expected nil, got %v", *gotNil)
	}
}