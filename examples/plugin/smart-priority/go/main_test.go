package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetState puts the package-level state into a known configuration.
func resetState(t *testing.T, cfg pluginConfig) {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	state.config = cfg
	state.auths = make(map[string]*authState)
}

// pick runs a scheduler pick over candidates and returns the decoded response.
func pick(t *testing.T, candidates []pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickResponse {
	t.Helper()
	raw, errMarshal := json.Marshal(pluginapi.SchedulerPickRequest{Candidates: candidates})
	if errMarshal != nil {
		t.Fatalf("marshal pick request: %v", errMarshal)
	}
	out, errPick := handleSchedulerPick(raw)
	if errPick != nil {
		t.Fatalf("handleSchedulerPick: %v", errPick)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("unmarshal envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("pick returned error envelope: %s", out)
	}
	var resp pluginapi.SchedulerPickResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("unmarshal pick response: %v", errUnmarshal)
	}
	return resp
}

// complete reports a terminal outcome for an auth.
func complete(t *testing.T, authID string, outcome pluginapi.RequestCompletionOutcome) {
	t.Helper()
	raw, errMarshal := json.Marshal(pluginapi.RequestCompletion{AuthID: authID, Outcome: outcome})
	if errMarshal != nil {
		t.Fatalf("marshal completion: %v", errMarshal)
	}
	if _, errComplete := handleRequestComplete(raw); errComplete != nil {
		t.Fatalf("handleRequestComplete: %v", errComplete)
	}
}

func effectivePriority(t *testing.T, authID string) int {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	tracked := state.auths[authID]
	if tracked == nil {
		t.Fatalf("no state tracked for %q", authID)
	}
	return tracked.effectivePriority
}

// TestPickPrefersHighestConfiguredPriority verifies the baseline behaviour
// before any failures are recorded.
func TestPickPrefersHighestConfiguredPriority(t *testing.T) {
	resetState(t, defaultConfig())
	resp := pick(t, []pluginapi.SchedulerAuthCandidate{
		{ID: "low", Priority: 1},
		{ID: "high", Priority: 5},
	})
	if resp.AuthID != "high" {
		t.Fatalf("expected high, got %q (delegate %q)", resp.AuthID, resp.DelegateBuiltin)
	}
}

// TestFailuresDowngradeAndShiftTraffic is the core scenario: a failing
// high-priority auth must lose to a healthy lower-priority one.
func TestFailuresDowngradeAndShiftTraffic(t *testing.T) {
	cfg := defaultConfig()
	cfg.FailureThreshold = 3
	cfg.MinPriority = 1
	resetState(t, cfg)

	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "primary", Priority: 5},
		{ID: "backup", Priority: 4},
	}

	// Seed both auths so completions have a configured baseline.
	if resp := pick(t, candidates); resp.AuthID != "primary" {
		t.Fatalf("expected primary first, got %q", resp.AuthID)
	}

	// Two failures stay below the threshold, so primary keeps winning.
	complete(t, "primary", pluginapi.RequestCompletionFailed)
	complete(t, "primary", pluginapi.RequestCompletionFailed)
	if resp := pick(t, candidates); resp.AuthID != "primary" {
		t.Fatalf("expected primary below threshold, got %q", resp.AuthID)
	}

	// The third failure crosses the threshold and drops primary to 4, which
	// ties backup. The tie resolves to the first candidate scanned.
	complete(t, "primary", pluginapi.RequestCompletionFailed)
	if got := effectivePriority(t, "primary"); got != 4 {
		t.Fatalf("expected primary effective priority 4, got %d", got)
	}

	// Three more failures drop primary to 3, below backup.
	complete(t, "primary", pluginapi.RequestCompletionFailed)
	complete(t, "primary", pluginapi.RequestCompletionFailed)
	complete(t, "primary", pluginapi.RequestCompletionFailed)
	if got := effectivePriority(t, "primary"); got != 3 {
		t.Fatalf("expected primary effective priority 3, got %d", got)
	}
	if resp := pick(t, candidates); resp.AuthID != "backup" {
		t.Fatalf("expected traffic to shift to backup, got %q", resp.AuthID)
	}
}

// TestDowngradeStopsAtMinPriority guards the lower bound.
func TestDowngradeStopsAtMinPriority(t *testing.T) {
	cfg := defaultConfig()
	cfg.FailureThreshold = 1
	cfg.MinPriority = 2
	resetState(t, cfg)

	candidates := []pluginapi.SchedulerAuthCandidate{{ID: "only", Priority: 5}}
	pick(t, candidates)
	for i := 0; i < 20; i++ {
		complete(t, "only", pluginapi.RequestCompletionFailed)
	}
	if got := effectivePriority(t, "only"); got != 2 {
		t.Fatalf("expected floor of 2, got %d", got)
	}
}

// TestSuccessesRestorePriority verifies recovery via sustained success.
func TestSuccessesRestorePriority(t *testing.T) {
	cfg := defaultConfig()
	cfg.FailureThreshold = 1
	cfg.SuccessResetCount = 2
	resetState(t, cfg)

	candidates := []pluginapi.SchedulerAuthCandidate{{ID: "auth", Priority: 5}}
	pick(t, candidates)
	complete(t, "auth", pluginapi.RequestCompletionFailed)
	if got := effectivePriority(t, "auth"); got != 4 {
		t.Fatalf("expected 4 after failure, got %d", got)
	}

	complete(t, "auth", pluginapi.RequestCompletionSucceeded)
	if got := effectivePriority(t, "auth"); got != 4 {
		t.Fatalf("expected 4 below success threshold, got %d", got)
	}
	complete(t, "auth", pluginapi.RequestCompletionSucceeded)
	if got := effectivePriority(t, "auth"); got != 5 {
		t.Fatalf("expected full restore to 5, got %d", got)
	}
}

// TestRejectedAndCanceledDoNotDowngrade confirms client-side outcomes are not
// blamed on the channel.
func TestRejectedAndCanceledDoNotDowngrade(t *testing.T) {
	cfg := defaultConfig()
	cfg.FailureThreshold = 1
	resetState(t, cfg)

	candidates := []pluginapi.SchedulerAuthCandidate{{ID: "auth", Priority: 5}}
	pick(t, candidates)
	complete(t, "auth", pluginapi.RequestCompletionRejected)
	complete(t, "auth", pluginapi.RequestCompletionCanceled)
	if got := effectivePriority(t, "auth"); got != 5 {
		t.Fatalf("expected priority untouched, got %d", got)
	}
}

// TestTimeBasedRecovery verifies a quiet period restores priority even without
// successful traffic.
func TestTimeBasedRecovery(t *testing.T) {
	cfg := defaultConfig()
	cfg.FailureThreshold = 1
	cfg.RecoveryIntervalSec = 60
	resetState(t, cfg)

	candidates := []pluginapi.SchedulerAuthCandidate{{ID: "auth", Priority: 5}}
	pick(t, candidates)
	complete(t, "auth", pluginapi.RequestCompletionFailed)
	complete(t, "auth", pluginapi.RequestCompletionFailed)
	if got := effectivePriority(t, "auth"); got != 3 {
		t.Fatalf("expected 3 after two failures, got %d", got)
	}

	// Backdate the last failure by two intervals; both levels should return.
	state.mu.Lock()
	state.auths["auth"].lastFailure = time.Now().Add(-150 * time.Second)
	state.mu.Unlock()

	pick(t, candidates)
	if got := effectivePriority(t, "auth"); got != 5 {
		t.Fatalf("expected time-based restore to 5, got %d", got)
	}
}

// TestDisabledCandidatesAreSkipped ensures a disabled auth is never selected.
func TestDisabledCandidatesAreSkipped(t *testing.T) {
	resetState(t, defaultConfig())
	resp := pick(t, []pluginapi.SchedulerAuthCandidate{
		{ID: "disabled", Priority: 9, Status: statusDisabled},
		{ID: "healthy", Priority: 1},
	})
	if resp.AuthID != "healthy" {
		t.Fatalf("expected healthy, got %q", resp.AuthID)
	}
}

// TestCompletionWithoutAuthIDIsIgnored covers hosts older than schema
// version 2, where AuthID is always empty.
func TestCompletionWithoutAuthIDIsIgnored(t *testing.T) {
	resetState(t, defaultConfig())
	raw, errMarshal := json.Marshal(pluginapi.RequestCompletion{Outcome: pluginapi.RequestCompletionFailed})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if _, errComplete := handleRequestComplete(raw); errComplete != nil {
		t.Fatalf("expected no error, got %v", errComplete)
	}
	state.mu.Lock()
	count := len(state.auths)
	state.mu.Unlock()
	if count != 0 {
		t.Fatalf("expected no tracked auths, got %d", count)
	}
}

// TestDisabledPluginDelegates verifies the passthrough path.
func TestDisabledPluginDelegates(t *testing.T) {
	cfg := defaultConfig()
	cfg.Enabled = false
	resetState(t, cfg)

	resp := pick(t, []pluginapi.SchedulerAuthCandidate{{ID: "auth", Priority: 5}})
	if resp.AuthID != "" {
		t.Fatalf("expected no direct pick, got %q", resp.AuthID)
	}
	if resp.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("expected fill-first delegate, got %q", resp.DelegateBuiltin)
	}
}

// TestConfigureRejectsOldSchema verifies the version guard.
func TestConfigureRejectsOldSchema(t *testing.T) {
	raw, errMarshal := json.Marshal(lifecycleRequest{SchemaVersion: 1})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if errConfigure := configure(raw); errConfigure == nil {
		t.Fatal("expected schema version 1 to be rejected")
	}
}
