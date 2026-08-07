// Command smart-priority is a CLIProxyAPI plugin that dynamically downgrades
// the effective priority of auth records that keep failing, and restores it as
// they recover.
//
// The built-in scheduler treats configured priority as static: a failing auth is
// put into cooldown and then returns at its original priority. This plugin keeps
// a per-auth effective priority that drops after consecutive failures and climbs
// back after sustained success or a quiet period.
//
// It combines two capabilities:
//   - Scheduler.Pick supplies candidates with their configured priority, and the
//     plugin answers with the auth holding the highest effective priority.
//   - RequestLifecyclePlugin.HandleRequestComplete reports the outcome together
//     with the AuthID the scheduler selected, closing the feedback loop.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// statusDisabled matches the host's auth.StatusDisabled wire value. Candidates
// in this state are intentionally turned off, so they are never selected.
const statusDisabled = "disabled"

// pluginConfig mirrors the YAML block configured for this plugin.
type pluginConfig struct {
	Enabled               bool `yaml:"enabled"`
	FailureThreshold      int  `yaml:"failure_threshold"`
	PriorityDowngradeStep int  `yaml:"priority_downgrade_step"`
	RecoveryIntervalSec   int  `yaml:"recovery_interval_sec"`
	MinPriority           int  `yaml:"min_priority"`
	SuccessResetCount     int  `yaml:"success_reset_count"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		Enabled:               true,
		FailureThreshold:      3,
		PriorityDowngradeStep: 1,
		RecoveryIntervalSec:   300,
		MinPriority:           1,
		SuccessResetCount:     5,
	}
}

// authState tracks the dynamic priority of one auth record.
type authState struct {
	// configuredPriority is the priority the host reports for this auth.
	configuredPriority int
	// effectivePriority is the priority this plugin currently applies.
	effectivePriority int
	consecutiveFails  int
	consecutiveSucc   int
	lastFailure       time.Time
}

type pluginState struct {
	mu     sync.Mutex
	config pluginConfig
	auths  map[string]*authState
}

var state = pluginState{
	config: defaultConfig(),
	auths:  make(map[string]*authState),
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// registration is the plugin.register / plugin.reconfigure reply. Field names
// match the host's rpcRegistration decoder.
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Scheduler              bool `json:"scheduler"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodRequestComplete:
		return handleRequestComplete(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// configure applies the host-supplied YAML block. The AuthID field this plugin
// relies on was added in schema version 2, so older hosts are rejected outright
// rather than silently degrading to a no-op.
func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if req.SchemaVersion < 2 {
		return fmt.Errorf("smart-priority requires host schema version 2 or newer")
	}

	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if cfg.FailureThreshold < 1 {
		return fmt.Errorf("failure_threshold must be greater than zero")
	}
	if cfg.PriorityDowngradeStep < 1 {
		return fmt.Errorf("priority_downgrade_step must be greater than zero")
	}
	if cfg.SuccessResetCount < 1 {
		return fmt.Errorf("success_reset_count must be greater than zero")
	}
	if cfg.RecoveryIntervalSec < 0 {
		return fmt.Errorf("recovery_interval_sec must not be negative")
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	state.config = cfg
	// Drop accumulated state so a reconfigure starts from configured priorities.
	state.auths = make(map[string]*authState)
	return nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "smart-priority",
			Version:          "0.1.0",
			Author:           "qmzz",
			GitHubRepository: "https://github.com/qmzz/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "When false, delegates every pick to the built-in scheduler."},
				{Name: "failure_threshold", Type: pluginapi.ConfigFieldTypeNumber, Description: "Consecutive failures before the effective priority drops (default: 3)."},
				{Name: "priority_downgrade_step", Type: pluginapi.ConfigFieldTypeNumber, Description: "Priority levels dropped or restored per adjustment (default: 1)."},
				{Name: "recovery_interval_sec", Type: pluginapi.ConfigFieldTypeNumber, Description: "Seconds without a failure before one priority level is restored (default: 300)."},
				{Name: "min_priority", Type: pluginapi.ConfigFieldTypeNumber, Description: "Lower bound for the effective priority (default: 1)."},
				{Name: "success_reset_count", Type: pluginapi.ConfigFieldTypeNumber, Description: "Consecutive successes before one priority level is restored (default: 5)."},
			},
		},
		Capabilities: registrationCapability{
			Scheduler:              true,
			RequestLifecyclePlugin: true,
		},
	}
}

// handleSchedulerPick selects the candidate with the highest effective
// priority. Candidates arrive as pluginapi.SchedulerPickRequest, which carries
// no JSON tags, so the wire format uses Go field names.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.config.Enabled || len(req.Candidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{
			DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst,
			Handled:         true,
		})
	}

	now := time.Now()
	bestID := ""
	bestPriority := 0
	for _, candidate := range req.Candidates {
		if candidate.ID == "" || candidate.Status == statusDisabled {
			continue
		}
		priority := state.effectivePriorityLocked(candidate, now)
		if bestID == "" || priority > bestPriority {
			bestID = candidate.ID
			bestPriority = priority
		}
	}

	// Every candidate was disabled; let the host apply its own availability
	// rules rather than guessing here.
	if bestID == "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{
			DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst,
			Handled:         true,
		})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: bestID, Handled: true})
}

// effectivePriorityLocked returns the priority to use for a candidate, seeding
// tracking state on first sight and applying time-based recovery. Callers must
// hold state.mu.
func (s *pluginState) effectivePriorityLocked(candidate pluginapi.SchedulerAuthCandidate, now time.Time) int {
	tracked := s.auths[candidate.ID]
	if tracked == nil {
		tracked = &authState{
			configuredPriority: candidate.Priority,
			effectivePriority:  candidate.Priority,
		}
		s.auths[candidate.ID] = tracked
	}

	// The host remains the source of truth for configured priority, so adopt
	// changes made through config reloads and rebase the effective value.
	if tracked.configuredPriority != candidate.Priority {
		delta := candidate.Priority - tracked.configuredPriority
		tracked.configuredPriority = candidate.Priority
		tracked.effectivePriority += delta
	}

	if tracked.effectivePriority > tracked.configuredPriority {
		tracked.effectivePriority = tracked.configuredPriority
	}
	if tracked.effectivePriority < s.config.MinPriority {
		tracked.effectivePriority = s.config.MinPriority
	}

	// Recover one level per quiet interval once failures have stopped.
	if s.config.RecoveryIntervalSec > 0 &&
		tracked.effectivePriority < tracked.configuredPriority &&
		tracked.consecutiveFails == 0 &&
		!tracked.lastFailure.IsZero() {
		interval := time.Duration(s.config.RecoveryIntervalSec) * time.Second
		for now.Sub(tracked.lastFailure) >= interval && tracked.effectivePriority < tracked.configuredPriority {
			tracked.effectivePriority += s.config.PriorityDowngradeStep
			tracked.lastFailure = tracked.lastFailure.Add(interval)
		}
		if tracked.effectivePriority >= tracked.configuredPriority {
			tracked.effectivePriority = tracked.configuredPriority
			tracked.lastFailure = time.Time{}
		}
	}

	return tracked.effectivePriority
}

// handleRequestComplete folds a terminal request outcome into the tracked state
// for the auth the scheduler selected. AuthID requires host schema version 2;
// on older hosts it is always empty and this handler does nothing.
func handleRequestComplete(raw []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if errUnmarshal := json.Unmarshal(raw, &completion); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if completion.AuthID == "" {
		// The request ended before auth selection, so no channel is at fault.
		return okEnvelope(struct{}{})
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	tracked := state.auths[completion.AuthID]
	if tracked == nil {
		// A completion without a preceding pick carries no configured
		// priority, so there is no baseline to adjust against.
		return okEnvelope(struct{}{})
	}

	switch completion.Outcome {
	case pluginapi.RequestCompletionSucceeded:
		tracked.consecutiveFails = 0
		tracked.consecutiveSucc++
		if tracked.consecutiveSucc >= state.config.SuccessResetCount {
			tracked.consecutiveSucc = 0
			if tracked.effectivePriority < tracked.configuredPriority {
				tracked.effectivePriority += state.config.PriorityDowngradeStep
				if tracked.effectivePriority >= tracked.configuredPriority {
					tracked.effectivePriority = tracked.configuredPriority
					tracked.lastFailure = time.Time{}
				}
			}
		}
	case pluginapi.RequestCompletionFailed:
		tracked.consecutiveSucc = 0
		tracked.consecutiveFails++
		tracked.lastFailure = time.Now()
		if tracked.consecutiveFails >= state.config.FailureThreshold {
			tracked.consecutiveFails = 0
			tracked.effectivePriority -= state.config.PriorityDowngradeStep
			if tracked.effectivePriority < state.config.MinPriority {
				tracked.effectivePriority = state.config.MinPriority
			}
		}
	default:
		// Rejected and canceled outcomes describe client or interceptor
		// behaviour, not channel health, so they leave the counters untouched.
	}

	return okEnvelope(struct{}{})
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
