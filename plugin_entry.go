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

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginName          = "quota-guard"
	pluginVersion       = "0.4.1"
	resourceStatusPath  = "/status"
	contentTypeJSON     = "application/json; charset=utf-8"
	contentTypeHTML     = "text/html; charset=utf-8"
	window5h            = "5h"
	window7d            = "7d"
	windowMonthly       = "monthly"
	defaultStateVersion = 1
)

var guard = newQuotaGuard(time.Now)

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

var callHostFunc = callHost

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
		writeResponse(response, errorEnvelope("invalid_method", "method is required", false))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error(), false))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	guard.shutdown()
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := guard.configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return guard.pickAuth(request)
	case pluginabi.MethodUsageHandle:
		return guard.handleUsage(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Routes: []managementRoute{
				{Method: http.MethodGet, Path: "/plugins/quota-guard/status", Description: "Returns quota-guard account status."},
				{Method: http.MethodGet, Path: "/plugins/quota-guard/config", Description: "Returns quota-guard plugin configuration."},
				{Method: http.MethodPatch, Path: "/plugins/quota-guard/config", Description: "Updates mutable quota-guard plugin configuration."},
				{Method: http.MethodPost, Path: "/plugins/quota-guard/refresh", Description: "Refreshes Codex quota snapshots for all or one account."},
				{Method: http.MethodPost, Path: "/plugins/quota-guard/reset-window", Description: "Resets local usage events for one account window."},
			},
			Resources: []managementResource{{
				Path:        resourceStatusPath,
				Menu:        "Quota Guard",
				Description: "Shows quota-guard account remaining estimates and affinity controls.",
			}},
		})
	case pluginabi.MethodManagementHandle:
		return guard.handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, false), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			Logo:             "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/main/docs/logo.png",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "min_remaining_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Minimum estimated remaining percentage to keep before moving to the next fill-first auth."},
				{Name: "sticky_current_auth_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Keep the current primary auth during short bursts while its non-inflight remaining is still above reserve."},
				{Name: "fail_when_all_low", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Return a retryable scheduler error when every candidate is below the reserve threshold."},
				{Name: "delegate_when_unconfigured", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{pluginapi.SchedulerBuiltinFillFirst, pluginapi.SchedulerBuiltinRoundRobin}, Description: "Built-in scheduler used when quota-guard delegates."},
				{Name: "state_file", Type: pluginapi.ConfigFieldTypeString, Description: "JSON file used to persist quota estimates and inflight reservations."},
				{Name: "pro_limit_multiplier", Type: pluginapi.ConfigFieldTypeNumber, Description: "Multiplier applied to local usage deltas when keeper reports planType=pro."},
				{Name: "quota_refresh_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable refresh of real Codex quota snapshots through the configured CPA/keeper endpoint."},
				{Name: "quota_refresh_trigger_endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "Optional HTTP endpoint used to trigger a keeper refresh task before reading quota."},
				{Name: "quota_refresh_trigger_wait_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Seconds to wait after triggering keeper refresh before reading quota."},
				{Name: "quota_refresh_endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "HTTP endpoint template used to refresh one auth quota. Supports {auth_index}, {auth_id}, and {provider}."},
				{Name: "request_error_status_override_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Allow request-scoped client errors such as context_too_large to keep an auth eligible when the host status is error but no cooldown is active."},
				{Name: "legacy_primary_auth", Type: pluginapi.ConfigFieldTypeString, Description: "Preferred auth ID or auth index for requests without the client affinity header."},
				{Name: "client_affinity_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable X-CPA-Client-ID group affinity scheduling."},
				{Name: "client_affinity_header", Type: pluginapi.ConfigFieldTypeString, Description: "Request header used as the stable client affinity identity."},
				{Name: "client_affinity_group_min_size", Type: pluginapi.ConfigFieldTypeInteger, Description: "Target minimum number of auths per affinity group."},
				{Name: "client_affinity_repeatable_auths", Type: pluginapi.ConfigFieldTypeString, Description: "Auth IDs or auth indexes allowed to appear in multiple automatic affinity groups, typically Pro accounts."},
				{Name: "client_affinity_api_key_map", Type: pluginapi.ConfigFieldTypeString, Description: "Optional client ID to Keeper numeric API key ID mapping used when scheduler metadata does not include api_key_id."},
				{Name: "client_affinity_exclusive_auths", Type: pluginapi.ConfigFieldTypeObject, Description: "Optional auth ID or auth index policies that lease an account to one client until its idle release period expires."},
				{Name: "client_affinity_min_load_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Keep eligible affinity groups from remaining empty or below the minimum traffic floor."},
				{Name: "client_affinity_min_binding_per_group", Type: pluginapi.ConfigFieldTypeInteger, Description: "Minimum number of client bindings for each eligible affinity group."},
				{Name: "client_affinity_min_traffic_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Minimum observed traffic share for each eligible affinity group."},
				{Name: "client_affinity_dormant_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Header-activity window used to distinguish dormant persistent bindings from active affinity clients."},
				{Name: "client_affinity_rebalance_api_key_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Use Keeper API key usage as the preferred client load source."},
				{Name: "client_affinity_rebalance_api_key_usage_endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "Keeper realtime API key usage endpoint with an {api_key_id} placeholder."},
				{Name: "client_affinity_rebalance_api_key_refresh_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Minimum interval between Keeper API key usage refreshes."},
				{Name: "client_affinity_rebalance_unknown_client_policy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"local-fallback", "observe-only"}, Description: "Fallback behavior when a client has no usable Keeper API key usage."},
				{Name: "client_affinity_rebalance_unattributed_policy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"observe-only"}, Description: "Behavior for shared auth usage that cannot be attributed to a group."},
				{Name: "client_affinity_rebalance_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Analyze Keeper usage and rebalance idle client bindings across affinity groups."},
				{Name: "client_affinity_rebalance_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"observe", "auto"}, Description: "Observe only or automatically apply eligible rebalance moves."},
				{Name: "client_affinity_rebalance_usage_endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "Keeper realtime usage endpoint used for 60 minute auth load snapshots."},
				{Name: "client_affinity_rebalance_fast_window_minutes", Type: pluginapi.ConfigFieldTypeNumber, Description: "Fast Keeper usage window used to detect bursts."},
				{Name: "client_affinity_rebalance_fast_weight", Type: pluginapi.ConfigFieldTypeNumber, Description: "Weight of the fast-window rate in predicted load."},
				{Name: "client_affinity_rebalance_overload_threshold", Type: pluginapi.ConfigFieldTypeNumber, Description: "Normalized pressure required to count an overload sample."},
				{Name: "client_affinity_rebalance_target_threshold", Type: pluginapi.ConfigFieldTypeNumber, Description: "Maximum normalized pressure for a migration target."},
				{Name: "weekly_budget_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Adjust long-term capacity from the official weekly quota trajectory."},
				{Name: "weekly_budget_timezone_offset_hours", Type: pluginapi.ConfigFieldTypeNumber, Description: "Fixed timezone offset used for daily T+1 weekly quota samples."},
				{Name: "weekly_budget_max_adjustment_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Maximum daily capacity adjustment above or below weekly usable capacity."},
				{Name: "proxy_restore_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Restore missing Codex auth proxy_url values after OAuth re-login."},
				{Name: "proxy_map_file", Type: pluginapi.ConfigFieldTypeString, Description: "Private JSON mapping keyed by stable account_id or email."},
				{Name: "proxy_restore_interval_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Interval for checking re-authenticated auth files."},
				{Name: "proxy_restore_settle_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Delay after an auth file change before restoring its proxy."},
				{Name: "proxy_restore_block_missing", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Temporarily exclude a mapped account while its proxy is being restored."},
				{Name: "proxy_restore_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Restore missing per-auth proxy_url values after OAuth re-login using the private proxy map."},
				{Name: "proxy_map_file", Type: pluginapi.ConfigFieldTypeString, Description: "Private JSON mapping keyed by stable account_id or email; never expose its proxy URLs in the status page."},
				{Name: "proxy_restore_interval_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Interval for checking re-authenticated Codex files for missing proxy_url values."},
				{Name: "proxy_restore_settle_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Wait after an auth file change before writing a proxy mapping, allowing OAuth persistence to settle."},
				{Name: "proxy_restore_block_missing", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Temporarily exclude mapped accounts while their missing proxy_url is being restored."},
			},
		},
		Capabilities: registrationCapabilities{
			Scheduler:     true,
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}

}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, retryable bool) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, Retryable: retryable}})
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

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
