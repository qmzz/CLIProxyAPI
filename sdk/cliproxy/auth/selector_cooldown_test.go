package auth

import (
	"testing"
	"time"
)

func TestCooldownBlocksHonorsQuotaRecoverWindow(t *testing.T) {
	now := time.Now()
	recoverAt := now.Add(24 * time.Hour)

	blocked, reason, next := cooldownBlocks(false, now.Add(-time.Minute), QuotaState{
		Exceeded:      true,
		Reason:        "quota",
		NextRecoverAt: recoverAt,
	}, now)
	if !blocked {
		t.Fatal("expected quota recover window to block selection")
	}
	if reason != blockReasonCooldown {
		t.Fatalf("reason = %v, want cooldown", reason)
	}
	if !next.Equal(recoverAt) {
		t.Fatalf("next = %v, want %v", next, recoverAt)
	}
}

func TestIsAuthBlockedForModelFallsBackToAuthQuota(t *testing.T) {
	now := time.Now()
	recoverAt := now.Add(12 * time.Hour)
	auth := &Auth{
		ID:       "xai-1",
		Provider: "xai",
		ModelStates: map[string]*ModelState{
			"grok-4.5-build-free": {
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: recoverAt,
				},
			},
		},
		Unavailable:    true,
		NextRetryAfter: recoverAt,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: recoverAt,
		},
	}
	blocked, reason, _ := isAuthBlockedForModel(auth, "grok-4.5", now)
	if !blocked {
		t.Fatal("expected auth-level quota fallback to block grok-4.5 route")
	}
	if reason != blockReasonCooldown {
		t.Fatalf("reason = %v, want cooldown", reason)
	}
}

func TestIsFreeUsageExhaustedError(t *testing.T) {
	if !isFreeUsageExhaustedError(&Error{Message: `{"code":"subscription:free-usage-exhausted"}`}) {
		t.Fatal("expected free-usage message match")
	}
	if isFreeUsageExhaustedError(&Error{Message: "rate_limit"}) {
		t.Fatal("did not expect generic rate limit to match")
	}
}
