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
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName          = "quota-guard"
	pluginVersion       = "0.3.0"
	resourceStatusPath  = "/status"
	contentTypeJSON     = "application/json; charset=utf-8"
	contentTypeHTML     = "text/html; charset=utf-8"
	window5h            = "5h"
	window7d            = "7d"
	windowMonthly       = "monthly"
	defaultStateVersion = 1
)

var guard = newQuotaGuard(time.Now)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type pluginConfig struct {
	Enabled                               bool                `yaml:"enabled" json:"enabled"`
	Priority                              int                 `yaml:"priority" json:"priority"`
	StateFile                             string              `yaml:"state_file" json:"state_file"`
	MinRemainingPercent                   float64             `yaml:"min_remaining_percent" json:"min_remaining_percent"`
	StickyCurrentAuthSeconds              int64               `yaml:"sticky_current_auth_seconds" json:"sticky_current_auth_seconds"`
	FailWhenAllLow                        bool                `yaml:"fail_when_all_low" json:"fail_when_all_low"`
	DelegateWhenUnconfigured              string              `yaml:"delegate_when_unconfigured" json:"delegate_when_unconfigured"`
	Default5hLimitScore                   float64             `yaml:"default_5h_limit_score" json:"default_5h_limit_score"`
	Default7dLimitScore                   float64             `yaml:"default_7d_limit_score" json:"default_7d_limit_score"`
	DefaultMonthlyLimitScore              float64             `yaml:"default_monthly_limit_score" json:"default_monthly_limit_score"`
	ProLimitMultiplier                    float64             `yaml:"pro_limit_multiplier" json:"pro_limit_multiplier"`
	InflightReserveScore                  float64             `yaml:"inflight_reserve_score" json:"inflight_reserve_score"`
	MaxInflightAgeSeconds                 int64               `yaml:"max_inflight_age_seconds" json:"max_inflight_age_seconds"`
	CountFailedRequests                   bool                `yaml:"count_failed_requests" json:"count_failed_requests"`
	InputWeight                           float64             `yaml:"input_weight" json:"input_weight"`
	OutputWeight                          float64             `yaml:"output_weight" json:"output_weight"`
	ReasoningWeight                       float64             `yaml:"reasoning_weight" json:"reasoning_weight"`
	CachedWeight                          float64             `yaml:"cached_weight" json:"cached_weight"`
	RequestScore                          float64             `yaml:"request_score" json:"request_score"`
	QuotaQueryURL                         string              `yaml:"quota_query_url" json:"quota_query_url,omitempty"`
	QuotaQueryMinIntervalSecs             int64               `yaml:"quota_query_min_interval_seconds" json:"quota_query_min_interval_seconds,omitempty"`
	QuotaRefreshEnabled                   bool                `yaml:"quota_refresh_enabled" json:"quota_refresh_enabled"`
	QuotaRefreshTriggerEndpoint           string              `yaml:"quota_refresh_trigger_endpoint" json:"quota_refresh_trigger_endpoint,omitempty"`
	QuotaRefreshTriggerWaitSecs           int64               `yaml:"quota_refresh_trigger_wait_seconds" json:"quota_refresh_trigger_wait_seconds"`
	QuotaRefreshEndpoint                  string              `yaml:"quota_refresh_endpoint" json:"quota_refresh_endpoint,omitempty"`
	QuotaRefreshIntervalSecs              int64               `yaml:"quota_refresh_interval_seconds" json:"quota_refresh_interval_seconds"`
	QuotaRefreshMinIntervalSecs           int64               `yaml:"quota_refresh_min_interval_per_auth_seconds" json:"quota_refresh_min_interval_per_auth_seconds"`
	QuotaRefreshTimeoutSecs               int64               `yaml:"quota_refresh_timeout_seconds" json:"quota_refresh_timeout_seconds"`
	QuotaRefreshOnStartup                 bool                `yaml:"quota_refresh_on_startup" json:"quota_refresh_on_startup"`
	QuotaSnapshotMaxAgeSecs               int64               `yaml:"quota_snapshot_max_age_seconds" json:"quota_snapshot_max_age_seconds"`
	ResourceActionsRequireManagementKey   bool                `yaml:"resource_actions_require_management_key" json:"resource_actions_require_management_key"`
	RequestErrorStatusOverrideEnabled     bool                `yaml:"request_error_status_override_enabled" json:"request_error_status_override_enabled"`
	ClientAffinityEnabled                 bool                `yaml:"client_affinity_enabled" json:"client_affinity_enabled"`
	ClientAffinityHeader                  string              `yaml:"client_affinity_header" json:"client_affinity_header"`
	ClientAffinityGroupMinSize            int                 `yaml:"client_affinity_group_min_size" json:"client_affinity_group_min_size"`
	ClientAffinityAssignmentMode          string              `yaml:"client_affinity_assignment_mode" json:"client_affinity_assignment_mode"`
	ClientAffinityStorePlainID            bool                `yaml:"client_affinity_store_plain_id" json:"client_affinity_store_plain_id"`
	ClientAffinityAutoWeightByQuota       bool                `yaml:"client_affinity_auto_weight_by_quota" json:"client_affinity_auto_weight_by_quota"`
	ClientAffinityGroups                  map[string][]string `yaml:"client_affinity_groups,omitempty" json:"client_affinity_groups,omitempty"`
	ClientAffinityRepeatableAuths         []string            `yaml:"client_affinity_repeatable_auths,omitempty" json:"client_affinity_repeatable_auths,omitempty"`
	ClientAffinityRebalanceEnabled        bool                `yaml:"client_affinity_rebalance_enabled" json:"client_affinity_rebalance_enabled"`
	ClientAffinityRebalanceMode           string              `yaml:"client_affinity_rebalance_mode" json:"client_affinity_rebalance_mode"`
	ClientAffinityRebalanceUsageURL       string              `yaml:"client_affinity_rebalance_usage_endpoint" json:"client_affinity_rebalance_usage_endpoint,omitempty"`
	ClientAffinityRebalanceIntervalSecs   int64               `yaml:"client_affinity_rebalance_interval_seconds" json:"client_affinity_rebalance_interval_seconds"`
	ClientAffinityRebalanceWindowMins     int64               `yaml:"client_affinity_rebalance_window_minutes" json:"client_affinity_rebalance_window_minutes"`
	ClientAffinityRebalanceIdleSecs       int64               `yaml:"client_affinity_rebalance_idle_seconds" json:"client_affinity_rebalance_idle_seconds"`
	ClientAffinityRebalanceCooldownSecs   int64               `yaml:"client_affinity_rebalance_cooldown_seconds" json:"client_affinity_rebalance_cooldown_seconds"`
	ClientAffinityManualCooldownSecs      int64               `yaml:"client_affinity_manual_move_cooldown_seconds" json:"client_affinity_manual_move_cooldown_seconds"`
	ClientAffinityRebalanceWarmupSecs     int64               `yaml:"client_affinity_rebalance_warmup_seconds" json:"client_affinity_rebalance_warmup_seconds"`
	ClientAffinityRebalanceMaxMoves       int                 `yaml:"client_affinity_rebalance_max_moves_per_cycle" json:"client_affinity_rebalance_max_moves_per_cycle"`
	ClientAffinityRebalanceMinLoadRatio   float64             `yaml:"client_affinity_rebalance_min_load_ratio" json:"client_affinity_rebalance_min_load_ratio"`
	ClientAffinityRebalanceMinImprove     float64             `yaml:"client_affinity_rebalance_min_improvement_percent" json:"client_affinity_rebalance_min_improvement_percent"`
	ClientAffinityRebalanceHistoryLimit   int                 `yaml:"client_affinity_rebalance_history_limit" json:"client_affinity_rebalance_history_limit"`
	ClientAffinityRebalanceFastWindowMins int64               `yaml:"client_affinity_rebalance_fast_window_minutes" json:"client_affinity_rebalance_fast_window_minutes"`
	ClientAffinityRebalanceFastWeight     float64             `yaml:"client_affinity_rebalance_fast_weight" json:"client_affinity_rebalance_fast_weight"`
	ClientAffinityRebalanceOverload       float64             `yaml:"client_affinity_rebalance_overload_threshold" json:"client_affinity_rebalance_overload_threshold"`
	ClientAffinityRebalanceTarget         float64             `yaml:"client_affinity_rebalance_target_threshold" json:"client_affinity_rebalance_target_threshold"`
	ClientAffinityRebalanceStreak         int                 `yaml:"client_affinity_rebalance_overload_consecutive" json:"client_affinity_rebalance_overload_consecutive"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		Enabled:                               true,
		Priority:                              100,
		StateFile:                             "plugins/quota-guard-state.json",
		MinRemainingPercent:                   10,
		StickyCurrentAuthSeconds:              120,
		FailWhenAllLow:                        true,
		DelegateWhenUnconfigured:              pluginapi.SchedulerBuiltinFillFirst,
		Default5hLimitScore:                   1000000,
		Default7dLimitScore:                   10000000,
		DefaultMonthlyLimitScore:              40000000,
		ProLimitMultiplier:                    20,
		InflightReserveScore:                  30000,
		MaxInflightAgeSeconds:                 1800,
		CountFailedRequests:                   false,
		InputWeight:                           1,
		OutputWeight:                          1,
		ReasoningWeight:                       1,
		CachedWeight:                          0.1,
		RequestScore:                          1,
		QuotaQueryMinIntervalSecs:             300,
		QuotaRefreshEnabled:                   true,
		QuotaRefreshTriggerEndpoint:           "http://cpa-usage-keeper:8080/cpa/api/v1/quota/refresh",
		QuotaRefreshTriggerWaitSecs:           2,
		QuotaRefreshEndpoint:                  "http://cpa-usage-keeper:8080/cpa/api/v1/quota/refresh/{auth_index}",
		QuotaRefreshIntervalSecs:              60,
		QuotaRefreshMinIntervalSecs:           30,
		QuotaRefreshTimeoutSecs:               10,
		QuotaRefreshOnStartup:                 true,
		QuotaSnapshotMaxAgeSecs:               900,
		ResourceActionsRequireManagementKey:   false,
		RequestErrorStatusOverrideEnabled:     true,
		ClientAffinityEnabled:                 false,
		ClientAffinityHeader:                  "X-CPA-Client-ID",
		ClientAffinityGroupMinSize:            2,
		ClientAffinityAssignmentMode:          "auto-with-overrides",
		ClientAffinityStorePlainID:            true,
		ClientAffinityAutoWeightByQuota:       true,
		ClientAffinityRepeatableAuths:         nil,
		ClientAffinityRebalanceEnabled:        false,
		ClientAffinityRebalanceMode:           "observe",
		ClientAffinityRebalanceUsageURL:       "http://cpa-usage-keeper:8080/cpa/api/v1/usage/overview/realtime?window=60m",
		ClientAffinityRebalanceIntervalSecs:   300,
		ClientAffinityRebalanceWindowMins:     60,
		ClientAffinityRebalanceIdleSecs:       30,
		ClientAffinityRebalanceCooldownSecs:   2700,
		ClientAffinityManualCooldownSecs:      86400,
		ClientAffinityRebalanceWarmupSecs:     3600,
		ClientAffinityRebalanceMaxMoves:       1,
		ClientAffinityRebalanceMinLoadRatio:   1.5,
		ClientAffinityRebalanceMinImprove:     15,
		ClientAffinityRebalanceHistoryLimit:   200,
		ClientAffinityRebalanceFastWindowMins: 15,
		ClientAffinityRebalanceFastWeight:     0.7,
		ClientAffinityRebalanceOverload:       1.25,
		ClientAffinityRebalanceTarget:         0.85,
		ClientAffinityRebalanceStreak:         3,
	}
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	Scheduler     bool `json:"scheduler"`
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes,omitempty"`
	Resources []managementResource `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type stateFile struct {
	Version          int                            `json:"version"`
	SavedAt          time.Time                      `json:"saved_at"`
	CurrentAuthID    string                         `json:"current_auth_id,omitempty"`
	CurrentAuthIndex string                         `json:"current_auth_index,omitempty"`
	CurrentRole      string                         `json:"current_role,omitempty"`
	LastSelectedAt   time.Time                      `json:"last_selected_at,omitempty"`
	Accounts         map[string]*accountState       `json:"accounts"`
	ClientBindings   map[string]*clientBindingState `json:"client_bindings,omitempty"`
	ManualGroups     map[string][]string            `json:"manual_groups,omitempty"`
	Groups           map[string]*affinityGroupState `json:"groups,omitempty"`
	GroupCurrent     map[string]*groupCurrentState  `json:"group_current,omitempty"`
	ClientActivity   []clientActivityEvent          `json:"client_activity,omitempty"`
	Rebalance        rebalanceState                 `json:"rebalance,omitempty"`
}

type accountState struct {
	AuthID                 string                             `json:"auth_id"`
	AuthIndex              string                             `json:"auth_index,omitempty"`
	Provider               string                             `json:"provider,omitempty"`
	Priority               int                                `json:"priority,omitempty"`
	Status                 string                             `json:"status,omitempty"`
	StatusMessage          string                             `json:"status_message,omitempty"`
	Disabled               bool                               `json:"disabled,omitempty"`
	Unavailable            bool                               `json:"unavailable,omitempty"`
	NextRetryAfter         time.Time                          `json:"next_retry_after,omitempty"`
	UnknownStatusSeen      int64                              `json:"unknown_status_seen,omitempty"`
	LastUnknownStatusAt    time.Time                          `json:"last_unknown_status_at,omitempty"`
	LastUnknownStatusLogAt time.Time                          `json:"last_unknown_status_log_at,omitempty"`
	Success                int64                              `json:"success,omitempty"`
	Failed                 int64                              `json:"failed,omitempty"`
	RecentRequests         []pluginapi.HostRecentRequestEntry `json:"recent_requests,omitempty"`
	Limits                 map[string]float64                 `json:"limits"`
	ActiveWindows          map[string]bool                    `json:"active_windows"`
	QuotaSnapshots         map[string]quotaWindowSnapshot     `json:"quota_snapshots,omitempty"`
	Events                 []usageEvent                       `json:"events,omitempty"`
	Inflight               []inflightReserve                  `json:"inflight,omitempty"`
	Calibration            map[string]calib                   `json:"calibration,omitempty"`
	LastUsageAt            time.Time                          `json:"last_usage_at,omitempty"`
	LastQueryAt            map[string]time.Time               `json:"last_query_at,omitempty"`
	LastQuotaRefreshAt     time.Time                          `json:"last_quota_refresh_at,omitempty"`
	LastQuotaRefreshError  string                             `json:"last_quota_refresh_error,omitempty"`
	LastResetAt            map[string]time.Time               `json:"last_reset_at,omitempty"`
}

type quotaWindowSnapshot struct {
	At               time.Time  `json:"at"`
	Source           string     `json:"source"`
	Limit            float64    `json:"limit,omitempty"`
	LimitScore       float64    `json:"limit_score,omitempty"`
	Remaining        float64    `json:"remaining,omitempty"`
	RemainingPercent float64    `json:"remaining_percent"`
	PlanType         string     `json:"plan_type,omitempty"`
	Label            string     `json:"label,omitempty"`
	Metric           string     `json:"metric,omitempty"`
	ResetAt          *time.Time `json:"reset_at,omitempty"`
}

type usageEvent struct {
	At     time.Time `json:"at"`
	Score  float64   `json:"score"`
	Model  string    `json:"model,omitempty"`
	Failed bool      `json:"failed,omitempty"`
}

type inflightReserve struct {
	At       time.Time `json:"at"`
	ClientID string    `json:"client_id,omitempty"`
	GroupID  string    `json:"group_id,omitempty"`
	AuthID   string    `json:"auth_id,omitempty"`
}

type calib struct {
	At                     time.Time `json:"at"`
	RemainingPercent       float64   `json:"remaining_percent"`
	UsedScoreAtCalibration float64   `json:"used_score_at_calibration"`
	Source                 string    `json:"source"`
}

type clientBindingState struct {
	ClientID         string    `json:"client_id"`
	GroupID          string    `json:"group_id"`
	Source           string    `json:"source,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	LastSeenAt       time.Time `json:"last_seen_at,omitempty"`
	LastAutoMoveAt   time.Time `json:"last_auto_move_at,omitempty"`
	LastManualMoveAt time.Time `json:"last_manual_move_at,omitempty"`
	LastMoveReason   string    `json:"last_move_reason,omitempty"`
}

type clientActivityEvent struct {
	At       time.Time `json:"at"`
	ClientID string    `json:"client_id"`
	GroupID  string    `json:"group_id"`
	AuthID   string    `json:"auth_id"`
	Kind     string    `json:"kind"`
	Score    float64   `json:"score,omitempty"`
}

type keeperUsageSnapshot struct {
	WindowStart time.Time                  `json:"window_start,omitempty"`
	WindowEnd   time.Time                  `json:"window_end,omitempty"`
	FetchedAt   time.Time                  `json:"fetched_at,omitempty"`
	AuthFiles   map[string]keeperUsageItem `json:"auth_files,omitempty"`
}

type keeperUsageItem struct {
	AuthIndex string  `json:"auth_index"`
	Label     string  `json:"label,omitempty"`
	Tokens    float64 `json:"tokens"`
	Requests  int64   `json:"requests"`
	Share     float64 `json:"share,omitempty"`
}

type rebalanceState struct {
	StartedAt       time.Time                 `json:"started_at,omitempty"`
	LastAnalysisAt  time.Time                 `json:"last_analysis_at,omitempty"`
	LastError       string                    `json:"last_error,omitempty"`
	KeeperUsage     keeperUsageSnapshot       `json:"keeper_usage,omitempty"`
	KeeperFastUsage keeperUsageSnapshot       `json:"keeper_fast_usage,omitempty"`
	Groups          map[string]groupLoadState `json:"groups,omitempty"`
	OverloadStreak  map[string]int            `json:"overload_streak,omitempty"`
	History         []rebalanceHistoryEntry   `json:"history,omitempty"`
}

type groupLoadState struct {
	GroupID           string  `json:"group_id"`
	Tokens            float64 `json:"tokens"`
	Requests          float64 `json:"requests"`
	Capacity          float64 `json:"capacity"`
	ActualShare       float64 `json:"actual_share"`
	TargetShare       float64 `json:"target_share"`
	LoadFactor        float64 `json:"load_factor"`
	Eligible          bool    `json:"eligible"`
	Reason            string  `json:"reason,omitempty"`
	FastTokens        float64 `json:"fast_tokens,omitempty"`
	SlowTokens        float64 `json:"slow_tokens,omitempty"`
	EffectiveCapacity float64 `json:"effective_capacity,omitempty"`
}

type rebalanceHistoryEntry struct {
	At                 time.Time `json:"at"`
	Action             string    `json:"action"`
	Result             string    `json:"result"`
	ClientID           string    `json:"client_id,omitempty"`
	FromGroup          string    `json:"from_group,omitempty"`
	ToGroup            string    `json:"to_group,omitempty"`
	Reason             string    `json:"reason"`
	SourceTokens       float64   `json:"source_tokens,omitempty"`
	TargetTokens       float64   `json:"target_tokens,omitempty"`
	SourceLoadFactor   float64   `json:"source_load_factor,omitempty"`
	TargetLoadFactor   float64   `json:"target_load_factor,omitempty"`
	IdleSeconds        int64     `json:"idle_seconds,omitempty"`
	EstimatedTokens    float64   `json:"estimated_tokens,omitempty"`
	ImprovementPercent float64   `json:"improvement_percent,omitempty"`
}

type affinityGroupState struct {
	ID            string    `json:"id"`
	Members       []string  `json:"members"`
	MainAuthID    string    `json:"main_auth_id,omitempty"`
	BackupAuthIDs []string  `json:"backup_auth_ids,omitempty"`
	Weight        float64   `json:"weight,omitempty"`
	Source        string    `json:"source,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

type groupCurrentState struct {
	AuthID         string    `json:"auth_id,omitempty"`
	AuthIndex      string    `json:"auth_index,omitempty"`
	CurrentRole    string    `json:"current_role,omitempty"`
	LastSelectedAt time.Time `json:"last_selected_at,omitempty"`
}

type quotaGuard struct {
	mu          sync.Mutex
	cfg         pluginConfig
	state       stateFile
	now         func() time.Time
	loadErr     error
	saveErr     error
	refreshStop chan struct{}
}

type statusResponse struct {
	Config           pluginConfig      `json:"config"`
	StateFile        string            `json:"state_file"`
	CurrentAuthID    string            `json:"current_auth_id,omitempty"`
	CurrentAuthIndex string            `json:"current_auth_index,omitempty"`
	CurrentRole      string            `json:"current_role,omitempty"`
	CurrentReason    string            `json:"current_reason,omitempty"`
	LastSelectedAt   time.Time         `json:"last_selected_at,omitempty"`
	LoadError        string            `json:"load_error,omitempty"`
	SaveError        string            `json:"save_error,omitempty"`
	Affinity         affinitySnapshot  `json:"affinity,omitempty"`
	Accounts         []accountSnapshot `json:"accounts"`
	AuthFiles        any               `json:"auth_files,omitempty"`
	GeneratedAt      time.Time         `json:"generated_at"`
}

type accountSnapshot struct {
	AuthID                string                             `json:"auth_id"`
	AuthIndex             string                             `json:"auth_index,omitempty"`
	HostMatched           bool                               `json:"host_matched"`
	Role                  string                             `json:"role,omitempty"`
	Provider              string                             `json:"provider,omitempty"`
	Priority              int                                `json:"priority,omitempty"`
	Status                string                             `json:"status,omitempty"`
	StatusMessage         string                             `json:"status_message,omitempty"`
	Disabled              bool                               `json:"disabled,omitempty"`
	Unavailable           bool                               `json:"unavailable,omitempty"`
	NextRetryAfter        time.Time                          `json:"next_retry_after,omitempty"`
	UnknownStatusSeen     int64                              `json:"unknown_status_seen,omitempty"`
	LastUnknownStatusAt   time.Time                          `json:"last_unknown_status_at,omitempty"`
	Eligible              bool                               `json:"eligible"`
	Reason                string                             `json:"reason,omitempty"`
	Limits                map[string]float64                 `json:"limits"`
	ActiveWindows         []string                           `json:"active_windows"`
	RemainingPercent      float64                            `json:"remaining_percent"`
	WindowRemaining       map[string]float64                 `json:"window_remaining"`
	Used                  map[string]float64                 `json:"used"`
	UsedSinceSnapshot     map[string]float64                 `json:"used_since_snapshot,omitempty"`
	QuotaSnapshots        map[string]quotaWindowSnapshot     `json:"quota_snapshots,omitempty"`
	QuotaMode             string                             `json:"quota_mode"`
	InflightCount         int                                `json:"inflight_count"`
	Success               int64                              `json:"success,omitempty"`
	Failed                int64                              `json:"failed,omitempty"`
	RecentRequests        []pluginapi.HostRecentRequestEntry `json:"recent_requests,omitempty"`
	LastUsageAt           time.Time                          `json:"last_usage_at,omitempty"`
	Calibration           map[string]calib                   `json:"calibration,omitempty"`
	LastQueryAt           map[string]time.Time               `json:"last_query_at,omitempty"`
	LastQuotaRefreshAt    time.Time                          `json:"last_quota_refresh_at,omitempty"`
	LastQuotaRefreshError string                             `json:"last_quota_refresh_error,omitempty"`
	LastResetAt           map[string]time.Time               `json:"last_reset_at,omitempty"`
	AffinityGroups        []string                           `json:"affinity_groups,omitempty"`
}

type affinitySnapshot struct {
	Enabled           bool                    `json:"enabled"`
	Header            string                  `json:"header,omitempty"`
	LegacyWhenMissing bool                    `json:"legacy_when_missing"`
	GroupMinSize      int                     `json:"group_min_size,omitempty"`
	Groups            []affinityGroupSnapshot `json:"groups,omitempty"`
	Bindings          []clientBindingSnapshot `json:"bindings,omitempty"`
	Rebalance         rebalanceState          `json:"rebalance,omitempty"`
	FastWindowMinutes int64                   `json:"fast_window_minutes,omitempty"`
}

type affinityGroupSnapshot struct {
	ID               string    `json:"id"`
	Members          []string  `json:"members"`
	MainAuthID       string    `json:"main_auth_id,omitempty"`
	BackupAuthIDs    []string  `json:"backup_auth_ids,omitempty"`
	Weight           float64   `json:"weight,omitempty"`
	Source           string    `json:"source,omitempty"`
	CurrentAuthID    string    `json:"current_auth_id,omitempty"`
	CurrentAuthIndex string    `json:"current_auth_index,omitempty"`
	LastSelectedAt   time.Time `json:"last_selected_at,omitempty"`
	Eligible         bool      `json:"eligible"`
	Reason           string    `json:"reason,omitempty"`
	BindingCount     int       `json:"binding_count,omitempty"`
	Tokens60m        float64   `json:"tokens_60m,omitempty"`
	ActualShare      float64   `json:"actual_share,omitempty"`
	TargetShare      float64   `json:"target_share,omitempty"`
	LoadFactor       float64   `json:"load_factor,omitempty"`
	MainCapacity     float64   `json:"main_capacity,omitempty"`
	FastTokens       float64   `json:"fast_tokens,omitempty"`
	SlowTokens       float64   `json:"slow_tokens,omitempty"`
	OverloadStreak   int       `json:"overload_streak,omitempty"`
}

type clientBindingSnapshot struct {
	ClientID         string    `json:"client_id"`
	GroupID          string    `json:"group_id"`
	Source           string    `json:"source,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	LastSeenAt       time.Time `json:"last_seen_at,omitempty"`
	UsageScore60m    float64   `json:"usage_score_60m,omitempty"`
	Picks60m         int       `json:"picks_60m,omitempty"`
	LastAutoMoveAt   time.Time `json:"last_auto_move_at,omitempty"`
	LastManualMoveAt time.Time `json:"last_manual_move_at,omitempty"`
	LastMoveReason   string    `json:"last_move_reason,omitempty"`
	CooldownUntil    time.Time `json:"cooldown_until,omitempty"`
}

type calibrateRequest struct {
	AuthID                 string   `json:"auth_id"`
	AuthIndex              string   `json:"auth_index"`
	Window                 string   `json:"window"`
	ActualRemainingPercent *float64 `json:"actual_remaining_percent"`
	RemainingPercent       *float64 `json:"remaining_percent"`
	Source                 string   `json:"source"`
}

type resetWindowRequest struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Window    string `json:"window"`
}

type queryCalibrateRequest struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Window    string `json:"window"`
}

type refreshRequest struct {
	AuthID       string `json:"auth_id"`
	AuthIndex    string `json:"auth_index"`
	All          bool   `json:"all"`
	Force        bool   `json:"force"`
	AuthJSONOnly bool   `json:"auth_json_only"`
}

type refreshResponse struct {
	Status    string          `json:"status"`
	Refreshed []refreshResult `json:"refreshed"`
	Snapshot  statusResponse  `json:"snapshot"`
}

type refreshResult struct {
	AuthID    string   `json:"auth_id,omitempty"`
	AuthIndex string   `json:"auth_index,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Skipped   bool     `json:"skipped,omitempty"`
	Error     string   `json:"error,omitempty"`
	Source    string   `json:"source,omitempty"`
	Windows   []string `json:"windows,omitempty"`
}

type quotaQueryResponse struct {
	AuthID           string   `json:"auth_id"`
	AuthIndex        string   `json:"auth_index"`
	Window           string   `json:"window"`
	RemainingPercent *float64 `json:"remaining_percent"`
}

type authListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type authGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
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
			GitHubRepository: "https://github.com/David-yanp/quota-guard-plugina",
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
				{Name: "client_affinity_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable X-CPA-Client-ID group affinity scheduling."},
				{Name: "client_affinity_header", Type: pluginapi.ConfigFieldTypeString, Description: "Request header used as the stable client affinity identity."},
				{Name: "client_affinity_group_min_size", Type: pluginapi.ConfigFieldTypeInteger, Description: "Target minimum number of auths per affinity group."},
				{Name: "client_affinity_repeatable_auths", Type: pluginapi.ConfigFieldTypeString, Description: "Auth IDs or auth indexes allowed to appear in multiple automatic affinity groups, typically Pro accounts."},
				{Name: "client_affinity_rebalance_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Analyze Keeper usage and rebalance idle client bindings across affinity groups."},
				{Name: "client_affinity_rebalance_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"observe", "auto"}, Description: "Observe only or automatically apply eligible rebalance moves."},
				{Name: "client_affinity_rebalance_usage_endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "Keeper realtime usage endpoint used for 60 minute auth load snapshots."},
				{Name: "client_affinity_rebalance_fast_window_minutes", Type: pluginapi.ConfigFieldTypeNumber, Description: "Fast Keeper usage window used to detect bursts."},
				{Name: "client_affinity_rebalance_fast_weight", Type: pluginapi.ConfigFieldTypeNumber, Description: "Weight of the fast-window rate in predicted load."},
				{Name: "client_affinity_rebalance_overload_threshold", Type: pluginapi.ConfigFieldTypeNumber, Description: "Normalized pressure required to count an overload sample."},
				{Name: "client_affinity_rebalance_target_threshold", Type: pluginapi.ConfigFieldTypeNumber, Description: "Maximum normalized pressure for a migration target."},
			},
		},
		Capabilities: registrationCapabilities{
			Scheduler:     true,
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}
}

func newQuotaGuard(now func() time.Time) *quotaGuard {
	return &quotaGuard{
		cfg: defaultConfig(),
		state: stateFile{
			Version:        defaultStateVersion,
			Accounts:       map[string]*accountState{},
			ClientBindings: map[string]*clientBindingState{},
			ManualGroups:   map[string][]string{},
			Groups:         map[string]*affinityGroupState{},
			GroupCurrent:   map[string]*groupCurrentState{},
		},
		now: now,
	}
}

func (g *quotaGuard) configure(raw []byte) error {
	cfg := defaultConfig()
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
		cfg = normalizeConfig(cfg)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cfg = cfg
	g.state = stateFile{
		Version:        defaultStateVersion,
		Accounts:       map[string]*accountState{},
		ClientBindings: map[string]*clientBindingState{},
		ManualGroups:   map[string][]string{},
		Groups:         map[string]*affinityGroupState{},
		GroupCurrent:   map[string]*groupCurrentState{},
	}
	g.loadErr = g.loadStateLocked()
	if g.state.Rebalance.StartedAt.IsZero() {
		g.state.Rebalance.StartedAt = g.now()
	}
	if g.state.Rebalance.Groups == nil {
		g.state.Rebalance.Groups = map[string]groupLoadState{}
	}
	if g.state.Rebalance.OverloadStreak == nil {
		g.state.Rebalance.OverloadStreak = map[string]int{}
	}
	g.saveErr = nil
	g.restartBackgroundRefreshLocked()
	return nil
}

func (g *quotaGuard) shutdown() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refreshStop != nil {
		close(g.refreshStop)
		g.refreshStop = nil
	}
}

func (g *quotaGuard) restartBackgroundRefreshLocked() {
	if g.refreshStop != nil {
		close(g.refreshStop)
		g.refreshStop = nil
	}
	if !g.cfg.Enabled || (!g.cfg.QuotaRefreshEnabled && !g.cfg.ClientAffinityRebalanceEnabled) {
		return
	}
	stop := make(chan struct{})
	g.refreshStop = stop
	intervalSecs := g.cfg.QuotaRefreshIntervalSecs
	if !g.cfg.QuotaRefreshEnabled || (g.cfg.ClientAffinityRebalanceEnabled && g.cfg.ClientAffinityRebalanceIntervalSecs < intervalSecs) {
		intervalSecs = g.cfg.ClientAffinityRebalanceIntervalSecs
	}
	if intervalSecs <= 0 {
		intervalSecs = 60
	}
	interval := time.Duration(intervalSecs) * time.Second
	onStartup := g.cfg.QuotaRefreshOnStartup
	go g.backgroundRefreshLoop(interval, onStartup, stop)
}

func (g *quotaGuard) backgroundRefreshLoop(interval time.Duration, onStartup bool, stop <-chan struct{}) {
	if onStartup {
		g.runBackgroundRefresh()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			g.runBackgroundRefresh()
			g.runBackgroundRebalance(false)
		case <-stop:
			return
		}
	}
}

func (g *quotaGuard) runBackgroundRefresh() {
	g.mu.Lock()
	enabled := g.cfg.QuotaRefreshEnabled
	g.mu.Unlock()
	if !enabled {
		return
	}
	auths, errAuths := callHostAuthList()
	if errAuths != nil {
		return
	}
	requests := g.backgroundRefreshRequests(auths.Files, g.now())
	if len(requests) == 0 {
		return
	}
	for _, req := range requests {
		g.refreshQuotaSnapshots(auths.Files, req)
	}
}

func (g *quotaGuard) backgroundRefreshRequest() (refreshRequest, bool) {
	requests := g.backgroundRefreshRequests(nil, g.now())
	if len(requests) == 0 {
		return refreshRequest{}, false
	}
	return requests[0], true
}

func (g *quotaGuard) backgroundRefreshRequests(files []pluginapi.HostAuthFileEntry, now time.Time) []refreshRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	requests := make([]refreshRequest, 0, 1)
	seen := map[string]int{}
	add := func(req refreshRequest) {
		key := strings.TrimSpace(req.AuthID)
		if key == "" {
			key = strings.TrimSpace(req.AuthIndex)
		}
		if key == "" {
			return
		}
		if pos, ok := seen[key]; ok {
			if req.Force {
				requests[pos].Force = true
			}
			if requests[pos].AuthID == "" {
				requests[pos].AuthID = req.AuthID
			}
			if requests[pos].AuthIndex == "" {
				requests[pos].AuthIndex = req.AuthIndex
			}
			return
		}
		seen[key] = len(requests)
		requests = append(requests, req)
	}
	authID := strings.TrimSpace(g.state.CurrentAuthID)
	authIndex := strings.TrimSpace(g.state.CurrentAuthIndex)
	if authID != "" || authIndex != "" {
		add(refreshRequest{AuthID: authID, AuthIndex: authIndex})
	}
	for _, file := range files {
		if !isCodexAuth(file) {
			continue
		}
		account := g.accountForHostFileLocked(file)
		if account == nil || !g.quotaResetRefreshDueLocked(account, now) {
			continue
		}
		add(refreshRequest{AuthID: account.AuthID, AuthIndex: account.AuthIndex, Force: true})
	}
	return requests
}

func (g *quotaGuard) accountForHostFileLocked(file pluginapi.HostAuthFileEntry) *accountState {
	id := strings.TrimSpace(file.ID)
	index := strings.TrimSpace(file.AuthIndex)
	for key, account := range g.state.Accounts {
		g.normalizeAccountLocked(account, key)
		if id != "" && strings.TrimSpace(account.AuthID) == id {
			return account
		}
		if index != "" && strings.TrimSpace(account.AuthIndex) == index {
			return account
		}
	}
	return nil
}

func (g *quotaGuard) quotaResetRefreshDueLocked(account *accountState, now time.Time) bool {
	if account == nil || account.QuotaSnapshots == nil {
		return false
	}
	for _, snap := range account.QuotaSnapshots {
		if snap.ResetAt == nil || snap.ResetAt.IsZero() || snap.ResetAt.After(now) {
			continue
		}
		if account.LastQuotaRefreshAt.IsZero() || account.LastQuotaRefreshAt.Before(*snap.ResetAt) {
			return true
		}
	}
	return false
}

func normalizeConfig(cfg pluginConfig) pluginConfig {
	defaults := defaultConfig()
	if cfg.StateFile == "" {
		cfg.StateFile = defaults.StateFile
	}
	if cfg.MinRemainingPercent <= 0 || cfg.MinRemainingPercent > 100 {
		cfg.MinRemainingPercent = defaults.MinRemainingPercent
	}
	if cfg.StickyCurrentAuthSeconds < 0 {
		cfg.StickyCurrentAuthSeconds = defaults.StickyCurrentAuthSeconds
	}
	if cfg.DelegateWhenUnconfigured == "" {
		cfg.DelegateWhenUnconfigured = defaults.DelegateWhenUnconfigured
	}
	if cfg.DelegateWhenUnconfigured != pluginapi.SchedulerBuiltinRoundRobin {
		cfg.DelegateWhenUnconfigured = pluginapi.SchedulerBuiltinFillFirst
	}
	if cfg.Default5hLimitScore <= 0 {
		cfg.Default5hLimitScore = defaults.Default5hLimitScore
	}
	if cfg.Default7dLimitScore <= 0 {
		cfg.Default7dLimitScore = defaults.Default7dLimitScore
	}
	if cfg.DefaultMonthlyLimitScore <= 0 {
		cfg.DefaultMonthlyLimitScore = defaults.DefaultMonthlyLimitScore
	}
	if cfg.ProLimitMultiplier <= 0 {
		cfg.ProLimitMultiplier = defaults.ProLimitMultiplier
	}
	if cfg.InflightReserveScore < 0 {
		cfg.InflightReserveScore = defaults.InflightReserveScore
	}
	if cfg.MaxInflightAgeSeconds <= 0 {
		cfg.MaxInflightAgeSeconds = defaults.MaxInflightAgeSeconds
	}
	if cfg.InputWeight == 0 {
		cfg.InputWeight = defaults.InputWeight
	}
	if cfg.OutputWeight == 0 {
		cfg.OutputWeight = defaults.OutputWeight
	}
	if cfg.ReasoningWeight == 0 {
		cfg.ReasoningWeight = defaults.ReasoningWeight
	}
	if cfg.CachedWeight == 0 {
		cfg.CachedWeight = defaults.CachedWeight
	}
	if cfg.RequestScore == 0 {
		cfg.RequestScore = defaults.RequestScore
	}
	if cfg.QuotaQueryMinIntervalSecs <= 0 {
		cfg.QuotaQueryMinIntervalSecs = defaults.QuotaQueryMinIntervalSecs
	}
	if cfg.QuotaRefreshIntervalSecs <= 0 {
		cfg.QuotaRefreshIntervalSecs = defaults.QuotaRefreshIntervalSecs
	}
	if cfg.QuotaRefreshTriggerWaitSecs < 0 {
		cfg.QuotaRefreshTriggerWaitSecs = defaults.QuotaRefreshTriggerWaitSecs
	}
	if cfg.QuotaRefreshMinIntervalSecs <= 0 {
		cfg.QuotaRefreshMinIntervalSecs = defaults.QuotaRefreshMinIntervalSecs
	}
	if cfg.QuotaRefreshTimeoutSecs <= 0 {
		cfg.QuotaRefreshTimeoutSecs = defaults.QuotaRefreshTimeoutSecs
	}
	if cfg.QuotaSnapshotMaxAgeSecs <= 0 {
		cfg.QuotaSnapshotMaxAgeSecs = defaults.QuotaSnapshotMaxAgeSecs
	}
	if cfg.QuotaRefreshEndpoint == "" {
		cfg.QuotaRefreshEndpoint = defaults.QuotaRefreshEndpoint
	}
	if strings.TrimSpace(cfg.ClientAffinityHeader) == "" {
		cfg.ClientAffinityHeader = defaults.ClientAffinityHeader
	}
	if cfg.ClientAffinityGroupMinSize <= 0 {
		cfg.ClientAffinityGroupMinSize = defaults.ClientAffinityGroupMinSize
	}
	if strings.TrimSpace(cfg.ClientAffinityAssignmentMode) == "" {
		cfg.ClientAffinityAssignmentMode = defaults.ClientAffinityAssignmentMode
	}
	if cfg.ClientAffinityGroups == nil {
		cfg.ClientAffinityGroups = map[string][]string{}
	}
	cfg.ClientAffinityRepeatableAuths = normalizeStringList(cfg.ClientAffinityRepeatableAuths)
	mode := strings.ToLower(strings.TrimSpace(cfg.ClientAffinityRebalanceMode))
	if mode != "auto" {
		mode = "observe"
	}
	cfg.ClientAffinityRebalanceMode = mode
	if strings.TrimSpace(cfg.ClientAffinityRebalanceUsageURL) == "" {
		cfg.ClientAffinityRebalanceUsageURL = defaults.ClientAffinityRebalanceUsageURL
	}
	if cfg.ClientAffinityRebalanceIntervalSecs <= 0 {
		cfg.ClientAffinityRebalanceIntervalSecs = defaults.ClientAffinityRebalanceIntervalSecs
	}
	if cfg.ClientAffinityRebalanceWindowMins <= 0 {
		cfg.ClientAffinityRebalanceWindowMins = defaults.ClientAffinityRebalanceWindowMins
	}
	if cfg.ClientAffinityRebalanceIdleSecs <= 0 {
		cfg.ClientAffinityRebalanceIdleSecs = defaults.ClientAffinityRebalanceIdleSecs
	}
	if cfg.ClientAffinityRebalanceCooldownSecs <= 0 {
		cfg.ClientAffinityRebalanceCooldownSecs = defaults.ClientAffinityRebalanceCooldownSecs
	}
	if cfg.ClientAffinityManualCooldownSecs <= 0 {
		cfg.ClientAffinityManualCooldownSecs = defaults.ClientAffinityManualCooldownSecs
	}
	if cfg.ClientAffinityRebalanceWarmupSecs < 0 {
		cfg.ClientAffinityRebalanceWarmupSecs = defaults.ClientAffinityRebalanceWarmupSecs
	}
	if cfg.ClientAffinityRebalanceMaxMoves <= 0 {
		cfg.ClientAffinityRebalanceMaxMoves = defaults.ClientAffinityRebalanceMaxMoves
	}
	if cfg.ClientAffinityRebalanceMaxMoves > 1 {
		cfg.ClientAffinityRebalanceMaxMoves = 1
	}
	if cfg.ClientAffinityRebalanceMinLoadRatio <= 1 {
		cfg.ClientAffinityRebalanceMinLoadRatio = defaults.ClientAffinityRebalanceMinLoadRatio
	}
	if cfg.ClientAffinityRebalanceMinImprove <= 0 {
		cfg.ClientAffinityRebalanceMinImprove = defaults.ClientAffinityRebalanceMinImprove
	}
	if cfg.ClientAffinityRebalanceHistoryLimit <= 0 {
		cfg.ClientAffinityRebalanceHistoryLimit = defaults.ClientAffinityRebalanceHistoryLimit
	}
	if cfg.ClientAffinityRebalanceFastWindowMins != 15 && cfg.ClientAffinityRebalanceFastWindowMins != 30 {
		cfg.ClientAffinityRebalanceFastWindowMins = defaults.ClientAffinityRebalanceFastWindowMins
	}
	if cfg.ClientAffinityRebalanceFastWeight <= 0 || cfg.ClientAffinityRebalanceFastWeight >= 1 {
		cfg.ClientAffinityRebalanceFastWeight = defaults.ClientAffinityRebalanceFastWeight
	}
	if cfg.ClientAffinityRebalanceOverload <= 1 {
		cfg.ClientAffinityRebalanceOverload = defaults.ClientAffinityRebalanceOverload
	}
	if cfg.ClientAffinityRebalanceTarget <= 0 || cfg.ClientAffinityRebalanceTarget >= 1 {
		cfg.ClientAffinityRebalanceTarget = defaults.ClientAffinityRebalanceTarget
	}
	if cfg.ClientAffinityRebalanceStreak <= 0 {
		cfg.ClientAffinityRebalanceStreak = defaults.ClientAffinityRebalanceStreak
	}
	return cfg
}

func (g *quotaGuard) pickAuth(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	resp, errPick := g.pick(req)
	if errPick != nil {
		return errorEnvelope("scheduler_quota_guard_exhausted", errPick.Error(), true), nil
	}
	return okEnvelope(resp)
}

func (g *quotaGuard) pick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	if schedulerNeedsHostStatus(req.Candidates) {
		if listed, errAuth := callHostAuthList(); errAuth == nil {
			g.ingestHostAuths(listed.Files, false)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cfg := g.cfg
	if !cfg.Enabled || len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: cfg.DelegateWhenUnconfigured}, nil
	}
	candidates := eligibleCandidates(req.Candidates)
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: cfg.DelegateWhenUnconfigured}, nil
	}
	now := g.now()
	for _, candidate := range candidates {
		account := g.ensureAccountLocked(candidate)
		g.observeCandidateStatusLocked(account, candidate, now)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		return candidates[i].ID < candidates[j].ID
	})
	if clientID, ok := g.clientAffinityID(req); ok {
		return g.pickAffinityLocked(candidates, clientID, now)
	}
	return g.pickLegacyLocked(candidates, now)
}

func schedulerNeedsHostStatus(candidates []pluginapi.SchedulerAuthCandidate) bool {
	for _, candidate := range candidates {
		status := strings.ToLower(strings.TrimSpace(candidate.Status))
		if status != "" && status != "active" && status != "disabled" && status != "unavailable" {
			return true
		}
	}
	return false
}

func (g *quotaGuard) pickLegacyLocked(candidates []pluginapi.SchedulerAuthCandidate, now time.Time) (pluginapi.SchedulerPickResponse, error) {
	if current, ok := g.currentPrimaryCandidateLocked(candidates, now); ok {
		return g.selectCandidateLocked(current, now), nil
	}
	for _, candidate := range candidates {
		account := g.ensureAccountLocked(candidate)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		eligible, _ := g.accountEligibleLocked(account, remaining, windowRemaining, now)
		if eligible {
			return g.selectCandidateLocked(candidate, now), nil
		}
	}
	if g.cfg.FailWhenAllLow {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("all %d eligible candidates are below %.2f%% remaining", len(candidates), g.cfg.MinRemainingPercent)
	}
	return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: g.cfg.DelegateWhenUnconfigured}, nil
}

func (g *quotaGuard) clientAffinityID(req pluginapi.SchedulerPickRequest) (string, bool) {
	if !g.cfg.ClientAffinityEnabled {
		return "", false
	}
	clientID := strings.TrimSpace(http.Header(req.Options.Headers).Get(g.cfg.ClientAffinityHeader))
	if clientID == "" {
		return "", false
	}
	return clientID, true
}

func (g *quotaGuard) pickAffinityLocked(candidates []pluginapi.SchedulerAuthCandidate, clientID string, now time.Time) (pluginapi.SchedulerPickResponse, error) {
	g.rebuildAffinityGroupsLocked(candidates, now)
	groupID, ok := g.boundAffinityGroupLocked(clientID, candidates, now)
	if !ok {
		var reason string
		groupID, reason, ok = g.assignAffinityGroupLocked(clientID, candidates, now)
		if !ok {
			if g.cfg.FailWhenAllLow {
				return pluginapi.SchedulerPickResponse{}, fmt.Errorf("no eligible affinity group for client %q: %s", clientID, reason)
			}
			return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: g.cfg.DelegateWhenUnconfigured}, nil
		}
	}
	g.upsertClientBindingLocked(clientID, groupID, "client_header", now)
	members := g.affinityGroupCandidatesLocked(groupID, candidates)
	if len(members) == 0 {
		return g.pickLegacyLocked(candidates, now)
	}
	if selected, ok := g.firstEligibleGroupCandidateLocked(members, now); ok {
		return g.selectGroupCandidateLocked(groupID, clientID, selected, now), nil
	}
	groupID, reason, ok := g.assignAffinityGroupLocked(clientID, candidates, now)
	if ok {
		g.upsertClientBindingLocked(clientID, groupID, "client_header", now)
		members = g.affinityGroupCandidatesLocked(groupID, candidates)
		if selected, selectedOK := g.firstEligibleGroupCandidateLocked(members, now); selectedOK {
			return g.selectGroupCandidateLocked(groupID, clientID, selected, now), nil
		}
	}
	if g.cfg.FailWhenAllLow {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("all affinity group candidates are below %.2f%% remaining: %s", g.cfg.MinRemainingPercent, reason)
	}
	return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: g.cfg.DelegateWhenUnconfigured}, nil
}

func (g *quotaGuard) firstEligibleGroupCandidateLocked(candidates []pluginapi.SchedulerAuthCandidate, now time.Time) (pluginapi.SchedulerAuthCandidate, bool) {
	for _, candidate := range candidates {
		account := g.ensureAccountLocked(candidate)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		eligible, _ := g.accountEligibleLocked(account, remaining, windowRemaining, now)
		if eligible {
			return candidate, true
		}
	}
	return pluginapi.SchedulerAuthCandidate{}, false
}

func (g *quotaGuard) currentPrimaryCandidateLocked(candidates []pluginapi.SchedulerAuthCandidate, now time.Time) (pluginapi.SchedulerAuthCandidate, bool) {
	if strings.TrimSpace(g.state.CurrentAuthID) == "" {
		return pluginapi.SchedulerAuthCandidate{}, false
	}
	for _, candidate := range candidates {
		if candidate.ID != g.state.CurrentAuthID {
			continue
		}
		account := g.ensureAccountLocked(candidate)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		eligible, _ := g.accountEligibleLocked(account, remaining, windowRemaining, now)
		if eligible {
			return candidate, true
		}
		return pluginapi.SchedulerAuthCandidate{}, false
	}
	return pluginapi.SchedulerAuthCandidate{}, false
}

func (g *quotaGuard) selectCandidateLocked(candidate pluginapi.SchedulerAuthCandidate, now time.Time) pluginapi.SchedulerPickResponse {
	account := g.ensureAccountLocked(candidate)
	g.pruneInflightLocked(account, now)
	account.Inflight = append(account.Inflight, inflightReserve{At: now})
	g.state.CurrentAuthID = candidate.ID
	g.state.CurrentAuthIndex = account.AuthIndex
	g.state.CurrentRole = "primary"
	g.state.LastSelectedAt = now
	g.saveErr = g.saveStateLocked()
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: candidate.ID}
}

func (g *quotaGuard) currentGroupPrimaryCandidateLocked(groupID string, candidates []pluginapi.SchedulerAuthCandidate, now time.Time) (pluginapi.SchedulerAuthCandidate, bool) {
	current := g.state.GroupCurrent[groupID]
	if current == nil || strings.TrimSpace(current.AuthID) == "" {
		return pluginapi.SchedulerAuthCandidate{}, false
	}
	for _, candidate := range candidates {
		if candidate.ID != current.AuthID {
			continue
		}
		account := g.ensureAccountLocked(candidate)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		eligible, _ := g.accountEligibleLocked(account, remaining, windowRemaining, now)
		if eligible {
			return candidate, true
		}
		return pluginapi.SchedulerAuthCandidate{}, false
	}
	return pluginapi.SchedulerAuthCandidate{}, false
}

func (g *quotaGuard) selectGroupCandidateLocked(groupID, clientID string, candidate pluginapi.SchedulerAuthCandidate, now time.Time) pluginapi.SchedulerPickResponse {
	account := g.ensureAccountLocked(candidate)
	g.pruneInflightLocked(account, now)
	account.Inflight = append(account.Inflight, inflightReserve{At: now, ClientID: clientID, GroupID: groupID, AuthID: candidate.ID})
	g.recordClientActivityLocked(clientActivityEvent{At: now, ClientID: clientID, GroupID: groupID, AuthID: candidate.ID, Kind: "pick"})
	g.state.GroupCurrent[groupID] = &groupCurrentState{
		AuthID:         candidate.ID,
		AuthIndex:      account.AuthIndex,
		CurrentRole:    "primary",
		LastSelectedAt: now,
	}
	g.state.CurrentAuthID = candidate.ID
	g.state.CurrentAuthIndex = account.AuthIndex
	g.state.CurrentRole = "primary"
	g.state.LastSelectedAt = now
	g.saveErr = g.saveStateLocked()
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: candidate.ID}
}

func (g *quotaGuard) rebuildAffinityGroupsLocked(candidates []pluginapi.SchedulerAuthCandidate, now time.Time) {
	g.ensureAffinityStateLocked()
	previousGroups := g.state.Groups
	next := map[string]*affinityGroupState{}
	bySelector := map[string]string{}
	order := map[string]int{}
	for i, candidate := range candidates {
		order[candidate.ID] = i
		account := g.ensureAccountLocked(candidate)
		bySelector[candidate.ID] = candidate.ID
		if account.AuthIndex != "" {
			bySelector[account.AuthIndex] = candidate.ID
		}
	}
	covered := map[string]bool{}
	for _, spec := range g.manualAffinityGroupSpecsLocked() {
		groupID := strings.TrimSpace(spec.ID)
		if groupID == "" {
			continue
		}
		members := uniqueCandidateIDs(spec.Selectors, bySelector)
		sort.SliceStable(members, func(i, j int) bool { return order[members[i]] < order[members[j]] })
		if len(members) == 0 {
			continue
		}
		for _, member := range members {
			covered[member] = true
		}
		mainAuthID, backupAuthIDs := affinityGroupRoles(members)
		next[groupID] = &affinityGroupState{
			ID:            groupID,
			Members:       members,
			MainAuthID:    mainAuthID,
			BackupAuthIDs: backupAuthIDs,
			Weight:        round2(g.affinityGroupWeightLocked(members, now)),
			Source:        spec.Source,
			UpdatedAt:     now,
		}
	}
	autoCandidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !covered[candidate.ID] {
			autoCandidates = append(autoCandidates, candidate)
		}
	}
	for _, group := range g.autoAffinityGroupsLocked(autoCandidates, now) {
		next[group.ID] = group
	}
	g.state.Groups = next
	g.migrateAutoAffinityBindingsLocked(previousGroups)
	for groupID, current := range g.state.GroupCurrent {
		if current == nil {
			delete(g.state.GroupCurrent, groupID)
			continue
		}
		if _, ok := g.state.Groups[groupID]; !ok {
			delete(g.state.GroupCurrent, groupID)
			continue
		}
		members := g.affinityGroupCandidatesLocked(groupID, candidates)
		selected, ok := g.firstEligibleGroupCandidateLocked(members, now)
		if !ok || current.AuthID != selected.ID {
			delete(g.state.GroupCurrent, groupID)
		}
	}
}

func (g *quotaGuard) migrateAutoAffinityBindingsLocked(previous map[string]*affinityGroupState) {
	if len(previous) == 0 || len(g.state.Groups) == 0 {
		return
	}
	mainToNew := map[string]string{}
	for groupID, group := range g.state.Groups {
		if group == nil || !strings.HasPrefix(strings.ToLower(groupID), "auto-") {
			continue
		}
		mainAuthID := strings.TrimSpace(firstNonEmpty(group.MainAuthID, firstString(group.Members)))
		if mainAuthID != "" {
			mainToNew[mainAuthID] = groupID
		}
	}
	oldToNew := map[string]string{}
	for oldID, oldGroup := range previous {
		if oldGroup == nil || !strings.HasPrefix(strings.ToLower(oldID), "auto-") {
			continue
		}
		oldMain := strings.TrimSpace(firstNonEmpty(oldGroup.MainAuthID, firstString(oldGroup.Members)))
		newID := mainToNew[oldMain]
		if newID != "" && newID != oldID {
			oldToNew[oldID] = newID
		}
	}
	for _, binding := range g.state.ClientBindings {
		if binding == nil {
			continue
		}
		if newID := oldToNew[binding.GroupID]; newID != "" {
			binding.GroupID = newID
			binding.UpdatedAt = g.now()
		}
	}
	for oldID, newID := range oldToNew {
		if current := g.state.GroupCurrent[oldID]; current != nil {
			if g.state.GroupCurrent[newID] == nil {
				g.state.GroupCurrent[newID] = current
			}
			delete(g.state.GroupCurrent, oldID)
		}
	}
}

type manualAffinityGroupSpec struct {
	ID        string
	Selectors []string
	Source    string
}

func (g *quotaGuard) manualAffinityGroupSpecsLocked() []manualAffinityGroupSpec {
	merged := map[string]manualAffinityGroupSpec{}
	for groupID, selectors := range g.cfg.ClientAffinityGroups {
		groupID = normalizeAffinityGroupID(groupID)
		selectors = normalizeStringList(selectors)
		if groupID == "" || len(selectors) == 0 {
			continue
		}
		merged[groupID] = manualAffinityGroupSpec{ID: groupID, Selectors: selectors, Source: "manual-config"}
	}
	for groupID, selectors := range g.state.ManualGroups {
		groupID = normalizeAffinityGroupID(groupID)
		selectors = normalizeStringList(selectors)
		if groupID == "" || len(selectors) == 0 {
			continue
		}
		merged[groupID] = manualAffinityGroupSpec{ID: groupID, Selectors: selectors, Source: "manual-state"}
	}
	out := make([]manualAffinityGroupSpec, 0, len(merged))
	for _, spec := range merged {
		out = append(out, spec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func uniqueCandidateIDs(selectors []string, bySelector map[string]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		id := bySelector[strings.TrimSpace(selector)]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func normalizeStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func normalizeAffinityGroupID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, r := range id {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.'
		if isAllowed {
			b.WriteRune(r)
			prevDash = r == '-'
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (g *quotaGuard) autoAffinityGroupsLocked(candidates []pluginapi.SchedulerAuthCandidate, now time.Time) []*affinityGroupState {
	if len(candidates) == 0 {
		return nil
	}
	minSize := g.cfg.ClientAffinityGroupMinSize
	if minSize <= 0 {
		minSize = 2
	}
	if minSize > len(candidates) {
		minSize = len(candidates)
	}
	repeatable := make([]string, 0, len(candidates))
	regular := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		account := g.ensureAccountLocked(candidate)
		if g.accountCanRepeatInAffinityLocked(account, now) {
			repeatable = append(repeatable, candidate.ID)
			continue
		}
		regular = append(regular, candidate.ID)
	}
	groups := make([]*affinityGroupState, 0, len(candidates))
	repeatIndex := 0
	nextRepeatable := func(exclude map[string]bool) string {
		if len(repeatable) == 0 {
			return ""
		}
		for attempts := 0; attempts < len(repeatable); attempts++ {
			id := repeatable[repeatIndex%len(repeatable)]
			repeatIndex++
			if !exclude[id] {
				return id
			}
		}
		return ""
	}

	if len(repeatable) == 0 && len(regular) > 0 {
		for i := 0; i < len(regular); i += minSize {
			end := i + minSize
			if end > len(regular) {
				end = len(regular)
			}
			members := append([]string(nil), regular[i:end]...)
			if len(members) == 1 && len(groups) > 0 {
				groups[len(groups)-1].Members = append(groups[len(groups)-1].Members, members[0])
				groups[len(groups)-1].MainAuthID, groups[len(groups)-1].BackupAuthIDs = affinityGroupRoles(groups[len(groups)-1].Members)
				groups[len(groups)-1].Weight = round2(g.affinityGroupWeightLocked(groups[len(groups)-1].Members, now))
				continue
			}
			groups = append(groups, g.newAutoAffinityGroupLocked(members[0], members, now))
		}
		return groups
	}

	for _, id := range regular {
		members := []string{id}
		exclude := map[string]bool{id: true}
		for len(members) < minSize {
			idRepeat := nextRepeatable(exclude)
			if idRepeat == "" {
				break
			}
			members = append(members, idRepeat)
			exclude[idRepeat] = true
		}
		groups = append(groups, g.newAutoAffinityGroupLocked(id, members, now))
	}
	if len(regular) == 0 {
		for _, id := range repeatable {
			exclude := map[string]bool{id: true}
			members := []string{id}
			for len(members) < minSize {
				idRepeat := nextRepeatable(exclude)
				if idRepeat == "" {
					break
				}
				members = append(members, idRepeat)
				exclude[idRepeat] = true
			}
			groups = append(groups, g.newAutoAffinityGroupLocked(id, members, now))
		}
	}
	return groups
}

func (g *quotaGuard) newAutoAffinityGroupLocked(anchor string, members []string, now time.Time) *affinityGroupState {
	mainAuthID, backupAuthIDs := affinityGroupRoles(members)
	return &affinityGroupState{
		ID:            stableAutoAffinityGroupID(g.ensureAccountByKeyLocked(anchor)),
		Members:       members,
		MainAuthID:    mainAuthID,
		BackupAuthIDs: backupAuthIDs,
		Weight:        round2(g.affinityGroupWeightLocked(members, now)),
		Source:        "auto",
		UpdatedAt:     now,
	}
}

func affinityGroupRoles(members []string) (string, []string) {
	if len(members) == 0 {
		return "", nil
	}
	return members[0], append([]string(nil), members[1:]...)
}

func stableAutoAffinityGroupID(account *accountState) string {
	selector := ""
	if account != nil {
		selector = strings.TrimSpace(account.AuthIndex)
		if selector == "" {
			selector = strings.TrimSpace(account.AuthID)
		}
	}
	if selector == "" {
		selector = "unknown"
	}
	normalized := normalizeAffinityGroupID(selector)
	if normalized == "" {
		normalized = shortStableID(selector)
	}
	if len(normalized) > 48 {
		normalized = normalized[:40] + "-" + shortStableID(selector)
	}
	return "auto-" + normalized
}

func shortStableID(value string) string {
	h := uint32(2166136261)
	for _, b := range []byte(value) {
		h ^= uint32(b)
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}

func (g *quotaGuard) accountCanRepeatInAffinityLocked(account *accountState, now time.Time) bool {
	if account == nil {
		return false
	}
	for _, selector := range g.cfg.ClientAffinityRepeatableAuths {
		if selector == account.AuthID || selector == account.AuthIndex {
			return true
		}
	}
	for window, snap := range account.QuotaSnapshots {
		planType := strings.ToLower(strings.TrimSpace(snap.PlanType))
		if strings.Contains(planType, "pro") {
			return true
		}
		switch window {
		case window5h:
			if snap.LimitScore >= g.cfg.Default5hLimitScore*g.cfg.ProLimitMultiplier {
				return true
			}
		case window7d:
			if snap.LimitScore >= g.cfg.Default7dLimitScore*g.cfg.ProLimitMultiplier {
				return true
			}
		}
	}
	_ = now
	return false
}

func (g *quotaGuard) affinitySlotCountLocked(account *accountState, now time.Time) int {
	weight := g.accountAffinityWeightLocked(account, now)
	switch {
	case weight >= g.cfg.DefaultMonthlyLimitScore:
		return 4
	case weight >= g.cfg.Default5hLimitScore*g.cfg.ProLimitMultiplier:
		return 4
	case weight >= g.cfg.Default7dLimitScore:
		return 2
	default:
		return 1
	}
}

func (g *quotaGuard) accountAffinityWeightLocked(account *accountState, now time.Time) float64 {
	if account == nil {
		return 1
	}
	weight := 0.0
	for _, window := range []string{window5h, window7d, windowMonthly} {
		if account.ActiveWindows[window] {
			weight = math.Max(weight, g.effectiveWindowLimitLocked(account, window, now))
		}
	}
	if weight <= 0 {
		return 1
	}
	return weight
}

func (g *quotaGuard) affinityGroupWeightLocked(members []string, now time.Time) float64 {
	total := 0.0
	for _, member := range members {
		account := g.ensureAccountByKeyLocked(member)
		total += g.accountAffinityWeightLocked(account, now)
	}
	if total <= 0 {
		return 1
	}
	return total
}

func (g *quotaGuard) boundAffinityGroupLocked(clientID string, candidates []pluginapi.SchedulerAuthCandidate, now time.Time) (string, bool) {
	g.ensureAffinityStateLocked()
	binding := g.state.ClientBindings[clientID]
	if binding == nil || strings.TrimSpace(binding.GroupID) == "" {
		return "", false
	}
	if g.affinityGroupHasEligibleLocked(binding.GroupID, candidates, now) {
		binding.LastSeenAt = now
		return binding.GroupID, true
	}
	return "", false
}

func (g *quotaGuard) assignAffinityGroupLocked(clientID string, candidates []pluginapi.SchedulerAuthCandidate, now time.Time) (string, string, bool) {
	bestGroup := ""
	bestScore := 0.0
	bestSet := false
	reason := "no groups"
	for groupID, group := range g.state.Groups {
		if group == nil {
			continue
		}
		eligible, groupReason := g.affinityGroupEligibleLocked(groupID, candidates, now)
		if !eligible {
			reason = groupReason
			continue
		}
		weight := group.Weight
		if g.cfg.ClientAffinityRebalanceEnabled && g.cfg.ClientAffinityRebalanceMode == "auto" && group.MainAuthID != "" {
			weight = g.effectiveAffinityCapacityLocked(g.ensureAccountByKeyLocked(group.MainAuthID), now)
			score := weightedRendezvousScore(clientID, groupID, weight)
			if !bestSet || score < bestScore || (score == bestScore && groupID < bestGroup) {
				bestSet = true
				bestScore = score
				bestGroup = groupID
			}
			continue
		}
		if weight <= 0 {
			weight = 1
		}
		score := float64(g.affinityBindingCountLocked(groupID)) / weight
		if !bestSet || score < bestScore || (score == bestScore && groupID < bestGroup) {
			bestSet = true
			bestScore = score
			bestGroup = groupID
		}
	}
	if !bestSet {
		return "", reason, false
	}
	return bestGroup, "", true
}

func weightedRendezvousScore(clientID, groupID string, weight float64) float64 {
	if weight <= 0 {
		weight = 1
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(clientID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(groupID))
	u := (float64(h.Sum64()) + 1) / (float64(^uint64(0)) + 2)
	return -math.Log(u) / weight
}

func (g *quotaGuard) upsertClientBindingLocked(clientID, groupID, source string, now time.Time) {
	g.ensureAffinityStateLocked()
	binding := g.state.ClientBindings[clientID]
	if binding == nil {
		binding = &clientBindingState{ClientID: clientID, CreatedAt: now}
		g.state.ClientBindings[clientID] = binding
	}
	if binding.GroupID != groupID {
		binding.GroupID = groupID
		binding.UpdatedAt = now
	}
	if binding.Source == "" {
		binding.Source = source
	}
	binding.LastSeenAt = now
}

func (g *quotaGuard) pruneClientBindingsLocked(now time.Time) bool {
	if len(g.state.ClientBindings) == 0 {
		return false
	}
	pruned := false
	for clientID, binding := range g.state.ClientBindings {
		if binding == nil {
			delete(g.state.ClientBindings, clientID)
			pruned = true
		}
	}
	_ = now
	return pruned
}

func (g *quotaGuard) affinityGroupCandidatesLocked(groupID string, candidates []pluginapi.SchedulerAuthCandidate) []pluginapi.SchedulerAuthCandidate {
	group := g.state.Groups[groupID]
	if group == nil {
		return nil
	}
	byID := map[string]pluginapi.SchedulerAuthCandidate{}
	for _, candidate := range candidates {
		byID[candidate.ID] = candidate
	}
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(group.Members))
	for _, member := range group.Members {
		if candidate, ok := byID[member]; ok {
			out = append(out, candidate)
		}
	}
	return out
}

func (g *quotaGuard) affinityGroupHasEligibleLocked(groupID string, candidates []pluginapi.SchedulerAuthCandidate, now time.Time) bool {
	eligible, _ := g.affinityGroupEligibleLocked(groupID, candidates, now)
	return eligible
}

func (g *quotaGuard) affinityGroupEligibleLocked(groupID string, candidates []pluginapi.SchedulerAuthCandidate, now time.Time) (bool, string) {
	members := g.affinityGroupCandidatesLocked(groupID, candidates)
	if len(members) == 0 {
		return false, "group has no current candidates"
	}
	lastReason := ""
	for _, candidate := range members {
		account := g.ensureAccountLocked(candidate)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		eligible, reason := g.accountEligibleLocked(account, remaining, windowRemaining, now)
		if eligible {
			return true, ""
		}
		lastReason = reason
	}
	return false, lastReason
}

func (g *quotaGuard) affinityBindingCountLocked(groupID string) int {
	count := 0
	for _, binding := range g.state.ClientBindings {
		if binding != nil && binding.GroupID == groupID {
			count++
		}
	}
	return count
}

func (g *quotaGuard) ensureAffinityStateLocked() {
	if g.state.ClientBindings == nil {
		g.state.ClientBindings = map[string]*clientBindingState{}
	}
	if g.state.Groups == nil {
		g.state.Groups = map[string]*affinityGroupState{}
	}
	if g.state.GroupCurrent == nil {
		g.state.GroupCurrent = map[string]*groupCurrentState{}
	}
}

func stringSliceContains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func eligibleCandidates(candidates []pluginapi.SchedulerAuthCandidate) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" || candidateDisabled(candidate) {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func candidateDisabled(candidate pluginapi.SchedulerAuthCandidate) bool {
	status := strings.ToLower(strings.TrimSpace(candidate.Status))
	switch status {
	case "disabled", "unavailable":
		return true
	}
	for key, value := range candidate.Attributes {
		k := strings.ToLower(strings.TrimSpace(key))
		v := strings.ToLower(strings.TrimSpace(value))
		if (k == "disabled" || k == "unavailable" || k == "status") && (v == "true" || v == "1" || v == "disabled" || v == "unavailable") {
			return true
		}
	}
	return false
}

func (g *quotaGuard) handleUsage(raw []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &rec); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	g.applyUsage(rec)
	return okEnvelope(map[string]any{})
}

func (g *quotaGuard) applyUsage(rec pluginapi.UsageRecord) {
	key := strings.TrimSpace(rec.AuthID)
	if key == "" {
		key = strings.TrimSpace(rec.AuthIndex)
	}
	if key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	account := g.ensureAccountByKeyLocked(key)
	if rec.AuthID != "" {
		account.AuthID = rec.AuthID
	}
	if rec.AuthIndex != "" {
		account.AuthIndex = rec.AuthIndex
	}
	reserve := g.releaseInflightLocked(account)
	if rec.Failed && !g.cfg.CountFailedRequests {
		g.saveErr = g.saveStateLocked()
		return
	}
	score := g.scoreUsage(rec)
	if score > 0 {
		eventAt := rec.RequestedAt
		if eventAt.IsZero() {
			eventAt = g.now()
		}
		account.Events = append(account.Events, usageEvent{At: eventAt, Score: score, Model: rec.Model, Failed: rec.Failed})
		account.LastUsageAt = eventAt
		if reserve.ClientID != "" && reserve.GroupID != "" {
			g.recordClientActivityLocked(clientActivityEvent{At: eventAt, ClientID: reserve.ClientID, GroupID: reserve.GroupID, AuthID: firstNonEmpty(reserve.AuthID, account.AuthID), Kind: "usage", Score: score})
		}
	}
	g.saveErr = g.saveStateLocked()
}

func (g *quotaGuard) scoreUsage(rec pluginapi.UsageRecord) float64 {
	d := rec.Detail
	score := float64(d.InputTokens)*g.cfg.InputWeight +
		float64(d.OutputTokens)*g.cfg.OutputWeight +
		float64(d.ReasoningTokens)*g.cfg.ReasoningWeight +
		float64(d.CachedTokens+d.CacheReadTokens+d.CacheCreationTokens)*g.cfg.CachedWeight +
		g.cfg.RequestScore
	if score <= 0 && d.TotalTokens > 0 {
		score = float64(d.TotalTokens) + g.cfg.RequestScore
	}
	if score < 0 {
		return 0
	}
	return score
}

func (g *quotaGuard) ensureAccountLocked(candidate pluginapi.SchedulerAuthCandidate) *accountState {
	account := g.ensureAccountByKeyLocked(candidate.ID)
	account.AuthID = candidate.ID
	account.Provider = strings.TrimSpace(candidate.Provider)
	account.Priority = candidate.Priority
	account.Status = strings.TrimSpace(candidate.Status)
	account.Disabled = candidateDisabled(candidate)
	if index := strings.TrimSpace(candidate.Attributes["auth_index"]); index != "" {
		account.AuthIndex = index
	}
	return account
}

func (g *quotaGuard) observeCandidateStatusLocked(account *accountState, candidate pluginapi.SchedulerAuthCandidate, now time.Time) {
	if account == nil || strings.TrimSpace(candidate.Status) != "" {
		return
	}
	account.UnknownStatusSeen++
	account.LastUnknownStatusAt = now
	if !account.LastUnknownStatusLogAt.IsZero() && now.Sub(account.LastUnknownStatusLogAt) < 10*time.Minute {
		return
	}
	account.LastUnknownStatusLogAt = now
	log.Printf(
		"quota-guard: scheduler candidate has empty status auth_id=%q auth_index=%q provider=%q priority=%d seen=%d",
		account.AuthID,
		account.AuthIndex,
		account.Provider,
		account.Priority,
		account.UnknownStatusSeen,
	)
}

func (g *quotaGuard) ensureAccountByKeyLocked(key string) *accountState {
	if g.state.Accounts == nil {
		g.state.Accounts = map[string]*accountState{}
	}
	if account := g.state.Accounts[key]; account != nil {
		g.normalizeAccountLocked(account, key)
		return account
	}
	account := &accountState{AuthID: key}
	g.normalizeAccountLocked(account, key)
	g.state.Accounts[key] = account
	return account
}

func (g *quotaGuard) normalizeAccountLocked(account *accountState, key string) {
	if account.AuthID == "" {
		account.AuthID = key
	}
	if account.Limits == nil {
		account.Limits = map[string]float64{}
	}
	if account.Limits[window5h] <= 0 {
		account.Limits[window5h] = g.cfg.Default5hLimitScore
	}
	if account.Limits[window7d] <= 0 {
		account.Limits[window7d] = g.cfg.Default7dLimitScore
	}
	if account.Limits[windowMonthly] <= 0 {
		account.Limits[windowMonthly] = g.cfg.DefaultMonthlyLimitScore
	}
	if account.ActiveWindows == nil {
		account.ActiveWindows = map[string]bool{window5h: true, window7d: true}
	}
	if account.QuotaSnapshots == nil {
		account.QuotaSnapshots = map[string]quotaWindowSnapshot{}
	}
	if !account.ActiveWindows[window5h] && !account.ActiveWindows[window7d] && !account.ActiveWindows[windowMonthly] {
		account.ActiveWindows[window5h] = true
		account.ActiveWindows[window7d] = true
	}
	if account.Calibration == nil {
		account.Calibration = map[string]calib{}
	}
	if account.LastQueryAt == nil {
		account.LastQueryAt = map[string]time.Time{}
	}
	if account.LastResetAt == nil {
		account.LastResetAt = map[string]time.Time{}
	}
}

func (g *quotaGuard) remainingPercentLocked(account *accountState, now time.Time) (float64, map[string]float64) {
	return g.remainingPercentWithInflightLocked(account, now, true)
}

func (g *quotaGuard) remainingPercentWithoutInflightLocked(account *accountState, now time.Time) (float64, map[string]float64) {
	return g.remainingPercentWithInflightLocked(account, now, false)
}

func (g *quotaGuard) remainingPercentWithInflightLocked(account *accountState, now time.Time, includeInflight bool) (float64, map[string]float64) {
	windowRemaining := map[string]float64{}
	active := false
	inflight := 0.0
	if includeInflight {
		inflight = g.inflightScore(account)
	}
	for _, window := range []string{window5h, window7d, windowMonthly} {
		if !account.ActiveWindows[window] {
			continue
		}
		active = true
		limit := account.Limits[window]
		if limit <= 0 {
			windowRemaining[window] = 0
			continue
		}
		used := g.usedScoreLocked(account, window, now)
		if snap, ok := usableQuotaSnapshot(account, window, now, g.cfg); ok {
			snapshotLimit := limit
			if snap.LimitScore > 0 {
				snapshotLimit = snap.LimitScore
			}
			remainingFromInflight := inflight / snapshotLimit * 100
			remaining := math.Max(0, snap.RemainingPercent-remainingFromInflight)
			windowRemaining[window] = round2(remaining)
			continue
		}
		remaining := math.Max(0, (limit-used-inflight)/limit*100)
		windowRemaining[window] = round2(remaining)
	}
	if !active {
		return 100, map[string]float64{}
	}
	return primaryRemaining(account, windowRemaining), windowRemaining
}

func primaryRemaining(account *accountState, windowRemaining map[string]float64) float64 {
	if account == nil {
		return 100
	}
	if account.ActiveWindows[windowMonthly] && !account.ActiveWindows[window5h] && !account.ActiveWindows[window7d] {
		return round2(windowRemaining[windowMonthly])
	}
	if account.ActiveWindows[window5h] {
		return round2(windowRemaining[window5h])
	}
	if account.ActiveWindows[windowMonthly] {
		return round2(windowRemaining[windowMonthly])
	}
	if account.ActiveWindows[window7d] {
		return round2(windowRemaining[window7d])
	}
	return 100
}

func usableQuotaSnapshot(account *accountState, window string, now time.Time, cfg pluginConfig) (quotaWindowSnapshot, bool) {
	if account == nil || account.QuotaSnapshots == nil {
		return quotaWindowSnapshot{}, false
	}
	snap, ok := account.QuotaSnapshots[window]
	if !ok || snap.At.IsZero() || snap.RemainingPercent < 0 {
		return quotaWindowSnapshot{}, false
	}
	return snap, true
}

func (g *quotaGuard) usedScoreLocked(account *accountState, window string, now time.Time) float64 {
	return g.usedScoreSinceLocked(account, window, cutoffForWindow(window, now), now)
}

func (g *quotaGuard) usedScoreSinceLocked(account *accountState, window string, since time.Time, now time.Time) float64 {
	var cutoff time.Time
	if since.IsZero() {
		cutoff = cutoffForWindow(window, now)
	} else {
		cutoff = since
	}
	var total float64
	for _, event := range account.Events {
		if event.At.IsZero() || !event.At.Before(cutoff) {
			total += event.Score
		}
	}
	return total
}

func (g *quotaGuard) inflightScore(account *accountState) float64 {
	return float64(len(account.Inflight)) * g.cfg.InflightReserveScore
}

func (g *quotaGuard) pruneInflightLocked(account *accountState, now time.Time) {
	if len(account.Inflight) == 0 {
		return
	}
	maxAge := time.Duration(g.cfg.MaxInflightAgeSeconds) * time.Second
	kept := account.Inflight[:0]
	for _, reserve := range account.Inflight {
		if reserve.At.IsZero() || now.Sub(reserve.At) <= maxAge {
			kept = append(kept, reserve)
		}
	}
	account.Inflight = kept
}

func (g *quotaGuard) releaseInflightLocked(account *accountState) inflightReserve {
	if len(account.Inflight) == 0 {
		return inflightReserve{}
	}
	reserve := account.Inflight[0]
	account.Inflight = account.Inflight[1:]
	return reserve
}

func (g *quotaGuard) recordClientActivityLocked(event clientActivityEvent) {
	if !g.cfg.ClientAffinityRebalanceEnabled {
		return
	}
	if strings.TrimSpace(event.ClientID) == "" || strings.TrimSpace(event.GroupID) == "" {
		return
	}
	g.state.ClientActivity = append(g.state.ClientActivity, event)
	g.pruneClientActivityLocked(g.now())
}

func (g *quotaGuard) pruneClientActivityLocked(now time.Time) {
	retention := time.Duration(g.cfg.ClientAffinityRebalanceWindowMins) * time.Minute
	if cooldown := time.Duration(g.cfg.ClientAffinityRebalanceCooldownSecs) * time.Second; cooldown > retention {
		retention = cooldown
	}
	if manual := time.Duration(g.cfg.ClientAffinityManualCooldownSecs) * time.Second; manual > retention {
		retention = manual
	}
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	cutoff := now.Add(-retention)
	kept := g.state.ClientActivity[:0]
	for _, event := range g.state.ClientActivity {
		if event.At.IsZero() || !event.At.Before(cutoff) {
			kept = append(kept, event)
		}
	}
	g.state.ClientActivity = kept
}

func (g *quotaGuard) calibrate(req calibrateRequest) error {
	key := strings.TrimSpace(req.AuthID)
	if key == "" {
		key = strings.TrimSpace(req.AuthIndex)
	}
	if key == "" {
		return fmt.Errorf("auth_id or auth_index is required")
	}
	window, errWindow := normalizeWindow(req.Window)
	if errWindow != nil {
		return errWindow
	}
	percent := req.RemainingPercent
	if percent == nil {
		percent = req.ActualRemainingPercent
	}
	if percent == nil {
		return fmt.Errorf("remaining_percent is required")
	}
	if *percent < 0 || *percent >= 100 {
		return fmt.Errorf("remaining_percent must be >= 0 and < 100")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	account := g.ensureAccountByKeyLocked(key)
	if req.AuthID != "" {
		account.AuthID = req.AuthID
	}
	if req.AuthIndex != "" {
		account.AuthIndex = req.AuthIndex
	}
	account.ActiveWindows[window] = true
	used := g.usedScoreLocked(account, window, now)
	if used > 0 {
		account.Limits[window] = math.Max(used/(1-(*percent/100)), 1)
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "manual"
	}
	account.Calibration[window] = calib{At: now, RemainingPercent: round2(*percent), UsedScoreAtCalibration: round2(used), Source: source}
	g.saveErr = g.saveStateLocked()
	return g.saveErr
}

func (g *quotaGuard) resetWindow(req resetWindowRequest) error {
	key := strings.TrimSpace(req.AuthID)
	if key == "" {
		key = strings.TrimSpace(req.AuthIndex)
	}
	if key == "" {
		return fmt.Errorf("auth_id or auth_index is required")
	}
	window, errWindow := normalizeWindow(req.Window)
	if errWindow != nil {
		return errWindow
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	account := g.ensureAccountByKeyLocked(key)
	now := g.now()
	cutoff := cutoffForWindow(window, now)
	kept := account.Events[:0]
	for _, event := range account.Events {
		if event.At.Before(cutoff) {
			kept = append(kept, event)
		}
	}
	account.Events = kept
	account.Inflight = nil
	account.LastResetAt[window] = now
	g.saveErr = g.saveStateLocked()
	return g.saveErr
}

func cutoffForWindow(window string, now time.Time) time.Time {
	switch window {
	case window5h:
		return now.Add(-5 * time.Hour)
	case window7d:
		return now.Add(-7 * 24 * time.Hour)
	case windowMonthly:
		return now.Add(-30 * 24 * time.Hour)
	default:
		return now
	}
}

func normalizeWindow(window string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case window5h, "5-hour", "5hour":
		return window5h, nil
	case window7d, "7-day", "7day":
		return window7d, nil
	case windowMonthly, "month", "30d":
		return windowMonthly, nil
	default:
		return "", fmt.Errorf("window must be one of 5h, 7d, monthly")
	}
}

func (g *quotaGuard) loadStateLocked() error {
	if strings.TrimSpace(g.cfg.StateFile) == "" {
		return nil
	}
	raw, errRead := os.ReadFile(g.cfg.StateFile)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil
		}
		return errRead
	}
	var loaded stateFile
	if errUnmarshal := json.Unmarshal(raw, &loaded); errUnmarshal != nil {
		return errUnmarshal
	}
	if loaded.Accounts == nil {
		loaded.Accounts = map[string]*accountState{}
	}
	if loaded.ClientBindings == nil {
		loaded.ClientBindings = map[string]*clientBindingState{}
	}
	if loaded.ManualGroups == nil {
		loaded.ManualGroups = map[string][]string{}
	}
	if loaded.Groups == nil {
		loaded.Groups = map[string]*affinityGroupState{}
	}
	if loaded.GroupCurrent == nil {
		loaded.GroupCurrent = map[string]*groupCurrentState{}
	}
	if loaded.Rebalance.Groups == nil {
		loaded.Rebalance.Groups = map[string]groupLoadState{}
	}
	if loaded.Rebalance.OverloadStreak == nil {
		loaded.Rebalance.OverloadStreak = map[string]int{}
	}
	if loaded.Rebalance.StartedAt.IsZero() {
		loaded.Rebalance.StartedAt = g.now()
	}
	for key, account := range loaded.Accounts {
		if account == nil {
			delete(loaded.Accounts, key)
			continue
		}
		g.normalizeAccountLocked(account, key)
	}
	for key, binding := range loaded.ClientBindings {
		if binding == nil || strings.TrimSpace(binding.GroupID) == "" {
			delete(loaded.ClientBindings, key)
			continue
		}
		if binding.ClientID == "" {
			binding.ClientID = key
		}
	}
	for groupID, members := range loaded.ManualGroups {
		normalizedID := normalizeAffinityGroupID(groupID)
		normalizedMembers := normalizeStringList(members)
		if normalizedID == "" || len(normalizedMembers) == 0 {
			delete(loaded.ManualGroups, groupID)
			continue
		}
		if normalizedID != groupID {
			delete(loaded.ManualGroups, groupID)
		}
		loaded.ManualGroups[normalizedID] = normalizedMembers
	}
	for key, group := range loaded.Groups {
		if group == nil || len(group.Members) == 0 {
			delete(loaded.Groups, key)
			continue
		}
		if group.ID == "" {
			group.ID = key
		}
		group.MainAuthID, group.BackupAuthIDs = affinityGroupRoles(group.Members)
	}
	for key, current := range loaded.GroupCurrent {
		if current == nil || strings.TrimSpace(current.AuthID) == "" {
			delete(loaded.GroupCurrent, key)
		}
	}
	if loaded.Version == 0 {
		loaded.Version = defaultStateVersion
	}
	g.state = loaded
	return nil
}

func (g *quotaGuard) saveStateLocked() error {
	if strings.TrimSpace(g.cfg.StateFile) == "" {
		return nil
	}
	g.state.Version = defaultStateVersion
	g.state.SavedAt = g.now()
	raw, errMarshal := json.MarshalIndent(g.state, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	dir := filepath.Dir(g.cfg.StateFile)
	if dir != "." && dir != "" {
		if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
			return errMkdir
		}
	}
	tmp := g.cfg.StateFile + ".tmp"
	if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, g.cfg.StateFile)
}

func (g *quotaGuard) snapshot(includeHostAuth bool) statusResponse {
	var auths authListResponse
	if includeHostAuth {
		if listed, errAuth := callHostAuthList(); errAuth == nil {
			auths = listed
			g.ingestHostAuths(listed.Files, false)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	resp := statusResponse{
		Config:           g.cfg,
		StateFile:        g.cfg.StateFile,
		CurrentAuthID:    g.state.CurrentAuthID,
		CurrentAuthIndex: g.state.CurrentAuthIndex,
		CurrentRole:      g.state.CurrentRole,
		LastSelectedAt:   g.state.LastSelectedAt,
		Affinity:         g.affinitySnapshotLocked(now),
		GeneratedAt:      now,
	}
	if g.loadErr != nil {
		resp.LoadError = g.loadErr.Error()
	}
	if g.saveErr != nil {
		resp.SaveError = g.saveErr.Error()
	}
	hostMatches := hostAuthMatchSet(auths.Files)
	currentSeen := false
	currentEligible := false
	currentReason := ""
	for key, account := range g.state.Accounts {
		g.normalizeAccountLocked(account, key)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		used := map[string]float64{}
		usedSinceSnapshot := map[string]float64{}
		for _, window := range []string{window5h, window7d, windowMonthly} {
			if account.ActiveWindows[window] {
				used[window] = round2(g.usedScoreLocked(account, window, now))
				if snap, ok := usableQuotaSnapshot(account, window, now, g.cfg); ok {
					usedSinceSnapshot[window] = round2(g.usedScoreSinceLocked(account, window, snap.At, now))
				}
			}
		}
		role := ""
		eligible, reason := g.accountEligibleLocked(account, remaining, windowRemaining, now)
		if account.AuthID != "" && account.AuthID == g.state.CurrentAuthID {
			currentSeen = true
			currentEligible = eligible
			currentReason = reason
			if eligible {
				role = "primary"
			} else {
				role = "last selected"
			}
		}
		resp.Accounts = append(resp.Accounts, accountSnapshot{
			AuthID:                account.AuthID,
			AuthIndex:             account.AuthIndex,
			HostMatched:           accountMatchesHost(account, hostMatches),
			Role:                  role,
			Provider:              account.Provider,
			Priority:              account.Priority,
			Status:                account.Status,
			StatusMessage:         account.StatusMessage,
			Disabled:              account.Disabled,
			Unavailable:           account.Unavailable,
			NextRetryAfter:        account.NextRetryAfter,
			UnknownStatusSeen:     account.UnknownStatusSeen,
			LastUnknownStatusAt:   account.LastUnknownStatusAt,
			Eligible:              eligible,
			Reason:                reason,
			Limits:                cloneFloatMap(account.Limits),
			ActiveWindows:         activeWindows(account),
			RemainingPercent:      remaining,
			WindowRemaining:       windowRemaining,
			Used:                  used,
			UsedSinceSnapshot:     usedSinceSnapshot,
			QuotaSnapshots:        cloneQuotaSnapshotMap(account.QuotaSnapshots),
			QuotaMode:             quotaMode(account, now, g.cfg),
			InflightCount:         len(account.Inflight),
			Success:               account.Success,
			Failed:                account.Failed,
			RecentRequests:        append([]pluginapi.HostRecentRequestEntry(nil), account.RecentRequests...),
			LastUsageAt:           account.LastUsageAt,
			Calibration:           cloneCalibMap(account.Calibration),
			LastQueryAt:           cloneTimeMap(account.LastQueryAt),
			LastQuotaRefreshAt:    account.LastQuotaRefreshAt,
			LastQuotaRefreshError: account.LastQuotaRefreshError,
			LastResetAt:           cloneTimeMap(account.LastResetAt),
			AffinityGroups:        g.accountAffinityGroupsLocked(account.AuthID),
		})
	}
	if resp.CurrentAuthID != "" {
		if currentSeen && currentEligible {
			resp.CurrentRole = "primary"
		} else {
			resp.CurrentRole = "stale primary"
			if currentReason != "" {
				resp.CurrentReason = currentReason
			} else if !currentSeen {
				resp.CurrentReason = "not in local account state"
			}
		}
	}
	sort.Slice(resp.Accounts, func(i, j int) bool {
		if resp.Accounts[i].Priority != resp.Accounts[j].Priority {
			return resp.Accounts[i].Priority > resp.Accounts[j].Priority
		}
		return resp.Accounts[i].AuthID < resp.Accounts[j].AuthID
	})
	if includeHostAuth {
		resp.AuthFiles = auths
	}
	return resp
}

func (g *quotaGuard) affinitySnapshotLocked(now time.Time) affinitySnapshot {
	g.ensureAffinityStateLocked()
	prunedBindings := g.pruneClientBindingsLocked(now)
	if g.cfg.ClientAffinityEnabled {
		g.rebuildAffinityGroupsLocked(g.affinitySnapshotCandidatesLocked(), now)
	}
	if prunedBindings {
		g.saveErr = g.saveStateLocked()
	}
	out := affinitySnapshot{
		Enabled:           g.cfg.ClientAffinityEnabled,
		Header:            g.cfg.ClientAffinityHeader,
		LegacyWhenMissing: true,
		GroupMinSize:      g.cfg.ClientAffinityGroupMinSize,
		Rebalance:         g.state.Rebalance,
		FastWindowMinutes: g.cfg.ClientAffinityRebalanceFastWindowMins,
	}
	groupIDs := make([]string, 0, len(g.state.Groups))
	for groupID := range g.state.Groups {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Strings(groupIDs)
	for _, groupID := range groupIDs {
		group := g.state.Groups[groupID]
		if group == nil {
			continue
		}
		current := g.state.GroupCurrent[groupID]
		currentAuthID := ""
		currentAuthIndex := ""
		lastSelectedAt := time.Time{}
		if current != nil {
			currentAuthID = current.AuthID
			currentAuthIndex = current.AuthIndex
			lastSelectedAt = current.LastSelectedAt
		}
		eligible, reason := g.affinityGroupEligibleFromStateLocked(groupID, now)
		load := g.state.Rebalance.Groups[groupID]
		out.Groups = append(out.Groups, affinityGroupSnapshot{
			ID:               group.ID,
			Members:          append([]string(nil), group.Members...),
			MainAuthID:       group.MainAuthID,
			BackupAuthIDs:    append([]string(nil), group.BackupAuthIDs...),
			Weight:           round2(group.Weight),
			Source:           group.Source,
			CurrentAuthID:    currentAuthID,
			CurrentAuthIndex: currentAuthIndex,
			LastSelectedAt:   lastSelectedAt,
			Eligible:         eligible,
			Reason:           reason,
			BindingCount:     g.affinityBindingCountLocked(groupID),
			Tokens60m:        round2(load.Tokens),
			ActualShare:      round2(load.ActualShare),
			TargetShare:      round2(load.TargetShare),
			LoadFactor:       round2(load.LoadFactor),
			MainCapacity:     round2(load.Capacity),
			FastTokens:       round2(load.FastTokens),
			SlowTokens:       round2(load.SlowTokens),
			OverloadStreak:   g.state.Rebalance.OverloadStreak[groupID],
		})
	}
	bindings := make([]*clientBindingState, 0, len(g.state.ClientBindings))
	for _, binding := range g.state.ClientBindings {
		if binding == nil {
			continue
		}
		bindings = append(bindings, binding)
	}
	sort.SliceStable(bindings, func(i, j int) bool {
		left := bindings[i]
		right := bindings[j]
		if !left.LastSeenAt.Equal(right.LastSeenAt) {
			return left.LastSeenAt.After(right.LastSeenAt)
		}
		return left.ClientID < right.ClientID
	})
	for _, binding := range bindings {
		usageScore := 0.0
		picks := 0
		windowStart := now.Add(-time.Duration(g.cfg.ClientAffinityRebalanceWindowMins) * time.Minute)
		for _, event := range g.state.ClientActivity {
			if event.ClientID != binding.ClientID || event.At.Before(windowStart) {
				continue
			}
			if event.Kind == "usage" {
				usageScore += event.Score
			} else if event.Kind == "pick" {
				picks++
			}
		}
		cooldownUntil := time.Time{}
		if !binding.LastAutoMoveAt.IsZero() {
			cooldownUntil = binding.LastAutoMoveAt.Add(time.Duration(g.cfg.ClientAffinityRebalanceCooldownSecs) * time.Second)
		}
		if !binding.LastManualMoveAt.IsZero() {
			manualUntil := binding.LastManualMoveAt.Add(time.Duration(g.cfg.ClientAffinityManualCooldownSecs) * time.Second)
			if manualUntil.After(cooldownUntil) {
				cooldownUntil = manualUntil
			}
		}
		out.Bindings = append(out.Bindings, clientBindingSnapshot{
			ClientID:         binding.ClientID,
			GroupID:          binding.GroupID,
			Source:           binding.Source,
			CreatedAt:        binding.CreatedAt,
			UpdatedAt:        binding.UpdatedAt,
			LastSeenAt:       binding.LastSeenAt,
			UsageScore60m:    round2(usageScore),
			Picks60m:         picks,
			LastAutoMoveAt:   binding.LastAutoMoveAt,
			LastManualMoveAt: binding.LastManualMoveAt,
			LastMoveReason:   binding.LastMoveReason,
			CooldownUntil:    cooldownUntil,
		})
	}
	return out
}

func (g *quotaGuard) affinitySnapshotCandidatesLocked() []pluginapi.SchedulerAuthCandidate {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(g.state.Accounts))
	for key, account := range g.state.Accounts {
		g.normalizeAccountLocked(account, key)
		if strings.TrimSpace(account.AuthID) == "" || account.Disabled {
			continue
		}
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{
			ID:       account.AuthID,
			Provider: account.Provider,
			Priority: account.Priority,
			Status:   firstNonEmpty(account.Status, "active"),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates
}

func (g *quotaGuard) affinityGroupEligibleFromStateLocked(groupID string, now time.Time) (bool, string) {
	group := g.state.Groups[groupID]
	if group == nil || len(group.Members) == 0 {
		return false, "empty group"
	}
	lastReason := ""
	for _, member := range group.Members {
		account := g.ensureAccountByKeyLocked(member)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		eligible, reason := g.accountEligibleLocked(account, remaining, windowRemaining, now)
		if eligible {
			return true, ""
		}
		lastReason = reason
	}
	return false, lastReason
}

func (g *quotaGuard) accountAffinityGroupsLocked(authID string) []string {
	if strings.TrimSpace(authID) == "" {
		return nil
	}
	var out []string
	for groupID, group := range g.state.Groups {
		if group != nil && stringSliceContains(group.Members, authID) {
			out = append(out, groupID)
		}
	}
	sort.Strings(out)
	return out
}

type hostAuthMatches struct {
	IDs     map[string]bool
	Indexes map[string]bool
}

func hostAuthMatchSet(files []pluginapi.HostAuthFileEntry) hostAuthMatches {
	out := hostAuthMatches{IDs: map[string]bool{}, Indexes: map[string]bool{}}
	for _, file := range files {
		if id := strings.TrimSpace(file.ID); id != "" {
			out.IDs[id] = true
		}
		if index := strings.TrimSpace(file.AuthIndex); index != "" {
			out.Indexes[index] = true
		}
	}
	return out
}

func accountMatchesHost(account *accountState, matches hostAuthMatches) bool {
	if account == nil {
		return false
	}
	if matches.IDs[strings.TrimSpace(account.AuthID)] {
		return true
	}
	if matches.Indexes[strings.TrimSpace(account.AuthIndex)] {
		return true
	}
	return false
}

func (g *quotaGuard) ingestHostAuths(files []pluginapi.HostAuthFileEntry, fetchQuota bool) {
	g.mu.Lock()
	for _, file := range files {
		key := strings.TrimSpace(file.ID)
		if key == "" {
			key = strings.TrimSpace(file.AuthIndex)
		}
		if key == "" {
			continue
		}
		account := g.ensureAccountByKeyLocked(key)
		applyHostAuthFile(account, file)
	}
	g.mu.Unlock()
	if !fetchQuota {
		return
	}
	for _, file := range files {
		if !isCodexAuth(file) || strings.TrimSpace(file.AuthIndex) == "" {
			continue
		}
		raw, errGet := callHostAuthGet(file.AuthIndex)
		if errGet != nil {
			continue
		}
		g.applyAuthJSONQuota(file, raw.JSON, "auth_json")
	}
}

func applyHostAuthFile(account *accountState, file pluginapi.HostAuthFileEntry) {
	if strings.TrimSpace(file.ID) != "" {
		account.AuthID = strings.TrimSpace(file.ID)
	}
	if strings.TrimSpace(file.AuthIndex) != "" {
		account.AuthIndex = strings.TrimSpace(file.AuthIndex)
	}
	account.Provider = firstNonEmpty(file.Provider, file.Type, account.Provider)
	account.Priority = file.Priority
	account.Status = strings.TrimSpace(file.Status)
	account.StatusMessage = strings.TrimSpace(file.StatusMessage)
	account.Disabled = file.Disabled
	account.Unavailable = file.Unavailable
	account.NextRetryAfter = file.NextRetryAfter
	account.Success = file.Success
	account.Failed = file.Failed
	account.RecentRequests = append([]pluginapi.HostRecentRequestEntry(nil), file.RecentRequests...)
}

func (g *quotaGuard) accountEligibleLocked(account *accountState, remaining float64, windowRemaining map[string]float64, now time.Time) (bool, string) {
	if account.Disabled {
		return false, "disabled"
	}
	if account.Unavailable {
		return false, "unavailable"
	}
	if account.NextRetryAfter.After(now) {
		return false, "cooldown until " + account.NextRetryAfter.UTC().Format(time.RFC3339)
	}
	switch strings.ToLower(strings.TrimSpace(account.Status)) {
	case "disabled", "unavailable":
		return false, strings.ToLower(strings.TrimSpace(account.Status))
	case "active":
	case "":
		return false, "status empty"
	default:
		if !g.requestErrorStatusOverrideAllowedLocked(account, now) {
			return false, "status " + strings.ToLower(strings.TrimSpace(account.Status))
		}
	}
	if remaining < g.cfg.MinRemainingPercent {
		return false, fmt.Sprintf("below %.2f%% reserve", g.cfg.MinRemainingPercent)
	}
	if reason := g.weeklyCapacityReasonLocked(account, windowRemaining, now); reason != "" {
		return false, reason
	}
	return true, ""
}

func (g *quotaGuard) requestErrorStatusOverrideAllowedLocked(account *accountState, now time.Time) bool {
	if !g.cfg.RequestErrorStatusOverrideEnabled || account == nil {
		return false
	}
	if account.Disabled || account.Unavailable || account.NextRetryAfter.After(now) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(account.Status), "error") {
		return false
	}
	return isRequestScopedStatusMessage(account.StatusMessage)
}

func isRequestScopedStatusMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	blocked := []string{
		"401",
		"429",
		"cloudflare",
		"challenge",
		"forbidden",
		"unauthorized",
		"quota",
		"rate limit",
		"rate_limit",
		"payment_required",
	}
	for _, token := range blocked {
		if strings.Contains(normalized, token) {
			return false
		}
	}
	allowed := []string{
		"context_too_large",
		"invalid_request_error",
		"context canceled",
		"context cancelled",
	}
	for _, token := range allowed {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

func (g *quotaGuard) weeklyCapacityReasonLocked(account *accountState, windowRemaining map[string]float64, now time.Time) string {
	if account == nil || !account.ActiveWindows[window5h] || !account.ActiveWindows[window7d] {
		return ""
	}
	fiveRemaining, okFive := windowRemaining[window5h]
	weeklyRemaining, okWeekly := windowRemaining[window7d]
	if !okFive || !okWeekly {
		return ""
	}
	fiveLimit := g.effectiveWindowLimitLocked(account, window5h, now)
	weeklyLimit := g.effectiveWindowLimitLocked(account, window7d, now)
	if fiveLimit <= 0 || weeklyLimit <= 0 {
		return ""
	}
	fiveUsableScore := math.Max(0, fiveRemaining-g.cfg.MinRemainingPercent) / 100 * fiveLimit
	if fiveUsableScore <= 0 {
		return ""
	}
	weeklyRemainingScore := weeklyRemaining / 100 * weeklyLimit
	if weeklyRemainingScore+0.0001 >= fiveUsableScore {
		return ""
	}
	return fmt.Sprintf("weekly %.2f%% cannot cover 5h-to-reserve", weeklyRemaining)
}

func (g *quotaGuard) effectiveWindowLimitLocked(account *accountState, window string, now time.Time) float64 {
	limit := account.Limits[window]
	if snap, ok := usableQuotaSnapshot(account, window, now, g.cfg); ok && snap.LimitScore > 0 {
		limit = snap.LimitScore
	}
	return limit
}

func quotaMode(account *accountState, now time.Time, cfg pluginConfig) string {
	if len(account.QuotaSnapshots) > 0 {
		if quotaSnapshotsStale(account, now, cfg) {
			return "real+usage stale"
		}
		return "real+usage"
	}
	return "estimated"
}

func quotaSnapshotsStale(account *accountState, now time.Time, cfg pluginConfig) bool {
	if account == nil || cfg.QuotaSnapshotMaxAgeSecs <= 0 {
		return false
	}
	for window, snap := range account.QuotaSnapshots {
		if !account.ActiveWindows[window] || snap.At.IsZero() {
			continue
		}
		if now.Sub(snap.At) > time.Duration(cfg.QuotaSnapshotMaxAgeSecs)*time.Second {
			return true
		}
	}
	return false
}

func cloneQuotaSnapshotMap(in map[string]quotaWindowSnapshot) map[string]quotaWindowSnapshot {
	out := map[string]quotaWindowSnapshot{}
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneFloatMap(in map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for key, value := range in {
		out[key] = round2(value)
	}
	return out
}

func cloneCalibMap(in map[string]calib) map[string]calib {
	out := map[string]calib{}
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneTimeMap(in map[string]time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for key, value := range in {
		out[key] = value
	}
	return out
}

func activeWindows(account *accountState) []string {
	var out []string
	for _, window := range []string{window5h, window7d, windowMonthly} {
		if account.ActiveWindows[window] {
			out = append(out, window)
		}
	}
	return out
}

func isCodexAuth(file pluginapi.HostAuthFileEntry) bool {
	provider := strings.ToLower(strings.TrimSpace(firstNonEmpty(file.Provider, file.Type)))
	return provider == "codex" || strings.HasPrefix(strings.ToLower(file.Name), "codex-")
}

func (g *quotaGuard) applyAuthJSONQuota(file pluginapi.HostAuthFileEntry, raw json.RawMessage, source string) refreshResult {
	result := refreshResult{AuthID: file.ID, AuthIndex: file.AuthIndex, Provider: firstNonEmpty(file.Provider, file.Type), Source: source}
	windows, refreshAt, errParse := parseCodexQuota(raw, source, g.now())
	if errParse != nil {
		result.Error = errParse.Error()
		g.recordQuotaRefreshResult(file, result)
		return result
	}
	result.Windows = mapKeysSorted(windows)
	g.mu.Lock()
	key := strings.TrimSpace(file.ID)
	if key == "" {
		key = strings.TrimSpace(file.AuthIndex)
	}
	account := g.ensureAccountByKeyLocked(key)
	applyHostAuthFile(account, file)
	for _, window := range []string{window5h, window7d, windowMonthly} {
		delete(account.QuotaSnapshots, window)
		account.ActiveWindows[window] = false
	}
	for window, snap := range windows {
		if snap.LimitScore <= 0 {
			snap.LimitScore = g.limitScoreForSnapshot(window, snap)
		}
		account.QuotaSnapshots[window] = snap
		account.ActiveWindows[window] = true
	}
	if !refreshAt.IsZero() {
		account.LastQuotaRefreshAt = refreshAt
	} else {
		account.LastQuotaRefreshAt = g.now()
	}
	account.LastQuotaRefreshError = ""
	g.saveErr = g.saveStateLocked()
	g.mu.Unlock()
	return result
}

func (g *quotaGuard) applyKeeperQuotaRefresh(file pluginapi.HostAuthFileEntry, raw []byte, source string) (refreshResult, bool) {
	result := refreshResult{AuthID: file.ID, AuthIndex: file.AuthIndex, Provider: firstNonEmpty(file.Provider, file.Type), Source: source}
	windows, refreshAt, errParse := parseKeeperQuotaRefresh(raw, source, g.now())
	if errParse != nil {
		return result, false
	}
	result.Windows = mapKeysSorted(windows)
	g.mu.Lock()
	key := strings.TrimSpace(file.ID)
	if key == "" {
		key = strings.TrimSpace(file.AuthIndex)
	}
	account := g.ensureAccountByKeyLocked(key)
	applyHostAuthFile(account, file)
	for _, window := range []string{window5h, window7d, windowMonthly} {
		delete(account.QuotaSnapshots, window)
		account.ActiveWindows[window] = false
	}
	for window, snap := range windows {
		if snap.LimitScore <= 0 {
			snap.LimitScore = g.limitScoreForSnapshot(window, snap)
		}
		account.QuotaSnapshots[window] = snap
		account.ActiveWindows[window] = true
	}
	if !refreshAt.IsZero() {
		account.LastQuotaRefreshAt = refreshAt
	} else {
		account.LastQuotaRefreshAt = g.now()
	}
	account.LastQuotaRefreshError = ""
	g.saveErr = g.saveStateLocked()
	g.mu.Unlock()
	return result, true
}

func (g *quotaGuard) recordQuotaRefreshResult(file pluginapi.HostAuthFileEntry, result refreshResult) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := strings.TrimSpace(file.ID)
	if key == "" {
		key = strings.TrimSpace(file.AuthIndex)
	}
	if key == "" {
		return
	}
	account := g.ensureAccountByKeyLocked(key)
	applyHostAuthFile(account, file)
	account.LastQuotaRefreshAt = g.now()
	account.LastQuotaRefreshError = result.Error
	g.saveErr = g.saveStateLocked()
}

func (g *quotaGuard) limitScoreForSnapshot(window string, snap quotaWindowSnapshot) float64 {
	base := g.cfg.Default7dLimitScore
	switch window {
	case window5h:
		base = g.cfg.Default5hLimitScore
	case windowMonthly:
		base = g.cfg.DefaultMonthlyLimitScore
	}
	planType := strings.ToLower(strings.TrimSpace(snap.PlanType))
	if strings.Contains(planType, "pro") {
		return base * g.cfg.ProLimitMultiplier
	}
	return base
}

func parseCodexQuota(raw json.RawMessage, source string, now time.Time) (map[string]quotaWindowSnapshot, time.Time, error) {
	var root map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &root); errUnmarshal != nil {
		return nil, time.Time{}, fmt.Errorf("decode auth json: %w", errUnmarshal)
	}
	quotaRaw := root["codex_quota"]
	if len(quotaRaw) == 0 {
		quotaRaw = root["quota"]
	}
	if len(quotaRaw) == 0 {
		return nil, time.Time{}, fmt.Errorf("auth json missing codex_quota")
	}
	var quota map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(quotaRaw, &quota); errUnmarshal != nil {
		return nil, time.Time{}, fmt.Errorf("decode codex_quota: %w", errUnmarshal)
	}
	refreshAt := parseTimeFromRaw(quota["last_refresh_at"])
	if refreshAt.IsZero() {
		refreshAt = parseTimeFromRaw(quota["probe_at"])
	}
	if refreshAt.IsZero() {
		refreshAt = time.Now().UTC()
	}
	detected := map[string]quotaWindowSnapshot{}
	for _, spec := range []struct {
		names  []string
		window string
	}{
		{names: []string{"five_hour", "5h", "fiveHour"}, window: window5h},
		{names: []string{"weekly", "seven_day", "7d", "week"}, window: window7d},
		{names: []string{"monthly", "month", "thirty_day", "30d"}, window: windowMonthly},
	} {
		for _, name := range spec.names {
			bucketRaw := quota[name]
			if len(bucketRaw) == 0 {
				continue
			}
			snap, ok := parseQuotaBucket(bucketRaw, refreshAt, source)
			if ok {
				detected[spec.window] = snap
				break
			}
		}
	}
	if monthly, ok := monthlyQuotaSnapshot(detected, now); ok {
		return map[string]quotaWindowSnapshot{windowMonthly: monthly}, refreshAt, nil
	}
	windows := detected
	if len(windows) == 0 {
		return nil, refreshAt, fmt.Errorf("codex_quota has no usable remaining/limit buckets")
	}
	return windows, refreshAt, nil
}

func parseKeeperQuotaRefresh(raw []byte, source string, now time.Time) (map[string]quotaWindowSnapshot, time.Time, error) {
	var root map[string]any
	if errUnmarshal := json.Unmarshal(raw, &root); errUnmarshal != nil {
		return nil, time.Time{}, fmt.Errorf("decode keeper quota refresh: %w", errUnmarshal)
	}
	refreshAt := time.Time{}
	if ts := parseTimePtrFromAny(root["refreshed_at"]); ts != nil {
		refreshAt = *ts
	}
	quotaObj, ok := root["quota"].(map[string]any)
	if !ok {
		return nil, refreshAt, fmt.Errorf("keeper quota response missing quota object")
	}
	items, ok := quotaObj["quota"].([]any)
	if !ok {
		return nil, refreshAt, fmt.Errorf("keeper quota response missing quota list")
	}
	windows := map[string]quotaWindowSnapshot{}
	windowPriority := map[string]int{}
	for _, item := range items {
		bucket, ok := item.(map[string]any)
		if !ok {
			continue
		}
		usedPercent, okUsed := numberValue(bucket["usedPercent"])
		if !okUsed {
			continue
		}
		resetAt := parseTimePtrFromAny(bucket["resetAt"])
		window := windowFromKeeperBucket(bucket, resetAt, now)
		if window == "" {
			continue
		}
		priority := keeperBucketPriority(bucket)
		if existingPriority, exists := windowPriority[window]; exists && priority < existingPriority {
			continue
		}
		snapshot := quotaWindowSnapshot{
			At:               firstNonZeroTime(refreshAt, now).UTC(),
			Source:           source,
			Limit:            100,
			Remaining:        round2(math.Max(0, math.Min(100, 100-usedPercent))),
			RemainingPercent: round2(math.Max(0, math.Min(100, 100-usedPercent))),
			PlanType:         strings.TrimSpace(stringFromAny(bucket["planType"])),
			Label:            strings.TrimSpace(stringFromAny(bucket["label"])),
			Metric:           strings.TrimSpace(stringFromAny(bucket["metric"])),
			ResetAt:          resetAt,
		}
		if existingPriority, exists := windowPriority[window]; exists && priority == existingPriority {
			if current, okCurrent := windows[window]; okCurrent && current.RemainingPercent <= snapshot.RemainingPercent {
				continue
			}
		}
		windows[window] = snapshot
		windowPriority[window] = priority
	}
	if len(windows) == 0 {
		return nil, refreshAt, fmt.Errorf("keeper quota response has no usable quota windows")
	}
	return windows, refreshAt, nil
}

func keeperBucketPriority(bucket map[string]any) int {
	scope := strings.ToLower(strings.TrimSpace(stringFromAny(bucket["scope"])))
	key := strings.ToLower(strings.TrimSpace(stringFromAny(bucket["key"])))
	switch {
	case scope == "window" || strings.HasPrefix(key, "rate_limit."):
		return 2
	case strings.HasPrefix(key, "additional_rate_limits."):
		return 1
	default:
		return 0
	}
}

func windowFromKeeperBucket(bucket map[string]any, resetAt *time.Time, now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	if resetAt != nil && resetAt.After(now.Add(8*24*time.Hour)) {
		return windowMonthly
	}
	if windowObj, ok := bucket["window"].(map[string]any); ok {
		if seconds, okSeconds := numberValue(windowObj["seconds"]); okSeconds {
			switch {
			case seconds > 8*24*60*60:
				return windowMonthly
			case seconds <= 6*60*60:
				return window5h
			default:
				return window7d
			}
		}
	}
	key := strings.ToLower(stringFromAny(bucket["key"]))
	switch {
	case strings.Contains(key, "primary"):
		return windowMonthly
	case strings.Contains(key, "5h") || strings.Contains(key, "five"):
		return window5h
	case strings.Contains(key, "week") || strings.Contains(key, "7d"):
		return window7d
	default:
		return ""
	}
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func monthlyQuotaSnapshot(windows map[string]quotaWindowSnapshot, now time.Time) (quotaWindowSnapshot, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	monthlyCutoff := now.Add(8 * 24 * time.Hour)
	for _, window := range []string{window5h, window7d} {
		snap, ok := windows[window]
		if !ok || snap.ResetAt == nil {
			continue
		}
		if snap.ResetAt.After(monthlyCutoff) {
			snap.Source = firstNonEmpty(snap.Source, "auth_json") + ":monthly-detected"
			return snap, true
		}
	}
	return quotaWindowSnapshot{}, false
}

func parseQuotaBucket(raw json.RawMessage, at time.Time, source string) (quotaWindowSnapshot, bool) {
	var bucket map[string]any
	if errUnmarshal := json.Unmarshal(raw, &bucket); errUnmarshal != nil {
		return quotaWindowSnapshot{}, false
	}
	limit, okLimit := numberValue(bucket["limit"])
	remaining, okRemaining := numberValue(bucket["remaining"])
	if !okLimit || !okRemaining || limit <= 0 {
		return quotaWindowSnapshot{}, false
	}
	resetAt := parseTimePtrFromAny(bucket["reset_at"])
	return quotaWindowSnapshot{
		At:               at.UTC(),
		Source:           source,
		Limit:            limit,
		Remaining:        remaining,
		RemainingPercent: round2(math.Max(0, math.Min(100, remaining/limit*100))),
		ResetAt:          resetAt,
	}, true
}

func parseTimeFromRaw(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var value any
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
		return time.Time{}
	}
	if ts := parseTimePtrFromAny(value); ts != nil {
		return *ts
	}
	return time.Time{}
}

func parseTimePtrFromAny(value any) *time.Time {
	s := strings.TrimSpace(stringFromAny(value))
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
		if parsed, errParse := time.Parse(layout, s); errParse == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

func mapKeysSorted[T any](in map[string]T) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (g *quotaGuard) handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
		}
	}
	path := normalizeManagementPath(req.Path)
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	switch {
	case method == http.MethodGet && (path == "/status" || path == resourceStatusPath || path == "/plugins/quota-guard/status"):
		if isResourcePath(req.Path) {
			if action := strings.ToLower(strings.TrimSpace(req.Query.Get("action"))); action != "" {
				return g.handleResourceAction(action, req.Query)
			}
		}
		if path == resourceStatusPath || strings.HasPrefix(req.Headers.Get("accept"), "text/html") {
			return okEnvelope(htmlResponse(http.StatusOK, renderStatusPage(g.snapshot(true))))
		}
		return okEnvelope(jsonResponse(http.StatusOK, g.snapshot(true)))
	case method == http.MethodGet && path == "/plugins/quota-guard/config":
		g.mu.Lock()
		cfg := g.cfg
		g.mu.Unlock()
		return okEnvelope(jsonResponse(http.StatusOK, cfg))
	case method == http.MethodPatch && path == "/plugins/quota-guard/config":
		return g.handleConfigPatch(req.Body)
	case method == http.MethodPost && (path == "/plugins/quota-guard/refresh" || path == "/refresh"):
		return g.handleRefresh(req.Body)
	case method == http.MethodPost && (path == "/plugins/quota-guard/reset-window" || path == "/reset-window"):
		body, errDecode := decodeResetWindowRequest(req.Body)
		if errDecode != nil {
			return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errDecode.Error()}))
		}
		if errReset := g.resetWindow(body); errReset != nil {
			return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errReset.Error()}))
		}
		return okEnvelope(jsonResponse(http.StatusOK, g.snapshot(false)))
	default:
		return okEnvelope(jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown quota-guard route"}))
	}
}

func isResourcePath(path string) bool {
	return strings.HasPrefix(strings.TrimSpace(path), "/v0/resource/plugins/quota-guard")
}

func (g *quotaGuard) handleResourceAction(action string, query url.Values) ([]byte, error) {
	switch action {
	case "refresh":
		req := refreshRequest{
			AuthID:    query.Get("auth_id"),
			AuthIndex: query.Get("auth_index"),
			All:       query.Get("all") == "true" || (query.Get("auth_id") == "" && query.Get("auth_index") == ""),
			Force:     query.Get("force") == "true",
		}
		auths, errAuths := callHostAuthList()
		if errAuths != nil {
			return okEnvelope(jsonResponse(http.StatusBadGateway, map[string]string{"error": errAuths.Error()}))
		}
		results := g.refreshQuotaSnapshots(auths.Files, req)
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"status": "ok", "refreshed": results}))
	case "delete-state":
		auths, errAuths := callHostAuthList()
		if errAuths != nil {
			return okEnvelope(jsonResponse(http.StatusBadGateway, map[string]string{"error": errAuths.Error()}))
		}
		if errDelete := g.deleteLocalAccountState(query.Get("auth_id"), query.Get("auth_index"), auths.Files); errDelete != nil {
			return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errDelete.Error()}))
		}
		return okEnvelope(jsonResponse(http.StatusOK, map[string]string{"status": "ok"}))
	case "save-manual-group":
		auths, errAuths := callHostAuthList()
		if errAuths != nil {
			return okEnvelope(jsonResponse(http.StatusBadGateway, map[string]string{"error": errAuths.Error()}))
		}
		members := query["member"]
		if len(members) == 0 {
			members = splitCSV(query.Get("members"))
		}
		if errSave := g.saveManualAffinityGroup(query.Get("group_id"), members, auths.Files); errSave != nil {
			return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errSave.Error()}))
		}
		return okEnvelope(jsonResponse(http.StatusOK, map[string]string{"status": "ok"}))
	case "delete-manual-group":
		if errDelete := g.deleteManualAffinityGroup(query.Get("group_id")); errDelete != nil {
			return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errDelete.Error()}))
		}
		return okEnvelope(jsonResponse(http.StatusOK, map[string]string{"status": "ok"}))
	case "delete-client-bindings":
		clientIDs := query["client_id"]
		if len(clientIDs) == 0 {
			clientIDs = splitCSV(query.Get("client_ids"))
		}
		deleted, errDelete := g.deleteClientBindings(clientIDs)
		if errDelete != nil {
			return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errDelete.Error()}))
		}
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"status": "ok", "deleted": deleted}))
	case "move-client-bindings":
		clientIDs := query["client_id"]
		if len(clientIDs) == 0 {
			clientIDs = splitCSV(query.Get("client_ids"))
		}
		moved, skipped, errMove := g.moveClientBindings(clientIDs, query.Get("group_id"))
		if errMove != nil {
			return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errMove.Error()}))
		}
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"status": "ok", "moved": moved, "skipped": skipped}))
	case "rebalance-analyze":
		entry, errAnalyze := g.runRebalanceNow(false)
		if errAnalyze != nil {
			return okEnvelope(jsonResponse(http.StatusBadGateway, map[string]any{"status": "error", "error": errAnalyze.Error(), "analysis": entry}))
		}
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"status": "ok", "analysis": entry}))
	case "rebalance-once":
		entry, errRebalance := g.runRebalanceNow(true)
		if errRebalance != nil {
			return okEnvelope(jsonResponse(http.StatusBadGateway, map[string]any{"status": "error", "error": errRebalance.Error(), "analysis": entry}))
		}
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"status": "ok", "analysis": entry}))
	default:
		return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": "unknown resource action"}))
	}
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

func (g *quotaGuard) saveManualAffinityGroup(groupID string, selectors []string, files []pluginapi.HostAuthFileEntry) error {
	groupID = normalizeAffinityGroupID(groupID)
	if groupID == "" {
		return fmt.Errorf("group_id is required")
	}
	if strings.HasPrefix(strings.ToLower(groupID), "auto-") {
		return fmt.Errorf("manual group id cannot start with auto-")
	}
	selectors = normalizeStringList(selectors)
	if len(selectors) < 2 {
		return fmt.Errorf("manual group needs at least 2 auths")
	}
	bySelector := map[string]string{}
	for _, file := range files {
		id := strings.TrimSpace(file.ID)
		index := strings.TrimSpace(file.AuthIndex)
		if id != "" {
			bySelector[id] = id
		}
		if index != "" && id != "" {
			bySelector[index] = id
		}
	}
	members := uniqueCandidateIDs(selectors, bySelector)
	if len(members) < 2 {
		return fmt.Errorf("manual group needs at least 2 existing host auths")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureAffinityStateLocked()
	if g.state.ManualGroups == nil {
		g.state.ManualGroups = map[string][]string{}
	}
	g.state.ManualGroups[groupID] = members
	g.rebuildAffinityGroupsLocked(g.affinitySnapshotCandidatesLocked(), g.now())
	g.saveErr = g.saveStateLocked()
	return g.saveErr
}

func (g *quotaGuard) deleteManualAffinityGroup(groupID string) error {
	groupID = normalizeAffinityGroupID(groupID)
	if groupID == "" {
		return fmt.Errorf("group_id is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.state.ManualGroups[groupID]; !ok {
		return fmt.Errorf("manual state group %q not found", groupID)
	}
	delete(g.state.ManualGroups, groupID)
	delete(g.state.GroupCurrent, groupID)
	g.rebuildAffinityGroupsLocked(g.affinitySnapshotCandidatesLocked(), g.now())
	g.saveErr = g.saveStateLocked()
	return g.saveErr
}

func (g *quotaGuard) deleteClientBindings(clientIDs []string) ([]string, error) {
	clientIDs = normalizeStringList(clientIDs)
	if len(clientIDs) == 0 {
		return nil, fmt.Errorf("client_id is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureAffinityStateLocked()
	deleted := make([]string, 0, len(clientIDs))
	for _, clientID := range clientIDs {
		if _, ok := g.state.ClientBindings[clientID]; ok {
			delete(g.state.ClientBindings, clientID)
			deleted = append(deleted, clientID)
		}
	}
	if len(deleted) == 0 {
		return nil, fmt.Errorf("no matching client bindings")
	}
	g.saveErr = g.saveStateLocked()
	return deleted, g.saveErr
}

func (g *quotaGuard) moveClientBindings(clientIDs []string, groupID string) ([]string, []string, error) {
	clientIDs = normalizeStringList(clientIDs)
	groupID = normalizeAffinityGroupID(groupID)
	if len(clientIDs) == 0 {
		return nil, nil, fmt.Errorf("client_id is required")
	}
	if groupID == "" {
		return nil, nil, fmt.Errorf("group_id is required")
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureAffinityStateLocked()
	if g.cfg.ClientAffinityEnabled {
		g.rebuildAffinityGroupsLocked(g.affinitySnapshotCandidatesLocked(), now)
	}
	if _, ok := g.state.Groups[groupID]; !ok {
		return nil, nil, fmt.Errorf("affinity group %q not found", groupID)
	}
	if eligible, reason := g.affinityGroupEligibleFromStateLocked(groupID, now); !eligible {
		if reason == "" {
			reason = "not eligible"
		}
		return nil, nil, fmt.Errorf("affinity group %q is not eligible: %s", groupID, reason)
	}
	moved := make([]string, 0, len(clientIDs))
	skipped := make([]string, 0, len(clientIDs))
	for _, clientID := range clientIDs {
		binding := g.state.ClientBindings[clientID]
		if binding == nil {
			continue
		}
		if binding.GroupID == groupID {
			binding.LastSeenAt = now
			skipped = append(skipped, clientID)
			continue
		}
		oldGroupID := binding.GroupID
		binding.GroupID = groupID
		binding.UpdatedAt = now
		binding.LastSeenAt = now
		binding.LastManualMoveAt = now
		binding.LastMoveReason = "manual move from " + firstNonEmpty(oldGroupID, "unknown") + " to " + groupID
		g.appendRebalanceHistoryLocked(rebalanceHistoryEntry{At: now, Action: "manual", Result: "moved", ClientID: clientID, FromGroup: oldGroupID, ToGroup: groupID, Reason: binding.LastMoveReason})
		moved = append(moved, clientID)
	}
	if len(moved) == 0 && len(skipped) == 0 {
		return nil, nil, fmt.Errorf("no matching client bindings")
	}
	g.saveErr = g.saveStateLocked()
	return moved, skipped, g.saveErr
}

type keeperRealtimeUsageResponse struct {
	Window       string    `json:"window"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
	CurrentUsage struct {
		AuthFiles []struct {
			Key      string  `json:"key"`
			Label    string  `json:"label"`
			Tokens   float64 `json:"tokens"`
			Requests int64   `json:"requests"`
			Share    float64 `json:"share"`
		} `json:"auth_files"`
	} `json:"current_usage"`
}

func (g *quotaGuard) runBackgroundRebalance(force bool) {
	g.mu.Lock()
	cfg := g.cfg
	now := g.now()
	due := force || g.state.Rebalance.LastAnalysisAt.IsZero() || now.Sub(g.state.Rebalance.LastAnalysisAt) >= time.Duration(cfg.ClientAffinityRebalanceIntervalSecs)*time.Second
	g.mu.Unlock()
	if !cfg.Enabled || !cfg.ClientAffinityEnabled || !cfg.ClientAffinityRebalanceEnabled || !due {
		return
	}
	_, _ = g.runRebalanceNow(force)
}

func (g *quotaGuard) runRebalanceNow(forceApply bool) (rebalanceHistoryEntry, error) {
	g.mu.Lock()
	cfg := g.cfg
	now := g.now()
	g.mu.Unlock()
	if auths, errAuths := callHostAuthList(); errAuths == nil {
		for _, req := range g.rebalanceQuotaRefreshRequests(auths.Files, now) {
			g.refreshQuotaSnapshots(auths.Files, req)
		}
	}
	fastURL := keeperUsageURLForWindow(cfg.ClientAffinityRebalanceUsageURL, cfg.ClientAffinityRebalanceFastWindowMins)
	slowURL := keeperUsageURLForWindow(cfg.ClientAffinityRebalanceUsageURL, cfg.ClientAffinityRebalanceWindowMins)
	fastSnapshot, errFast := fetchKeeperUsageSnapshot(fastURL, now)
	slowSnapshot, errSlow := fetchKeeperUsageSnapshot(slowURL, now)
	g.mu.Lock()
	defer g.mu.Unlock()
	if errFast != nil || errSlow != nil {
		errFetch := errFast
		if errFetch == nil {
			errFetch = errSlow
		}
		entry := g.recordRebalanceFailureLocked(now, "keeper usage unavailable: "+errFetch.Error())
		g.saveErr = g.saveStateLocked()
		return entry, errFetch
	}
	entry := g.analyzeRebalanceWindowsLocked(fastSnapshot, slowSnapshot, forceApply)
	g.saveErr = g.saveStateLocked()
	return entry, g.saveErr
}

func keeperUsageURLForWindow(endpoint string, minutes int64) string {
	parsed, errParse := url.Parse(strings.TrimSpace(endpoint))
	if errParse != nil || parsed == nil {
		return endpoint
	}
	query := parsed.Query()
	query.Set("window", fmt.Sprintf("%dm", minutes))
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func (g *quotaGuard) rebalanceQuotaRefreshRequests(files []pluginapi.HostAuthFileEntry, now time.Time) []refreshRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, file := range files {
		key := strings.TrimSpace(firstNonEmpty(file.ID, file.AuthIndex))
		if key == "" {
			continue
		}
		applyHostAuthFile(g.ensureAccountByKeyLocked(key), file)
	}
	g.rebuildAffinityGroupsLocked(g.affinitySnapshotCandidatesLocked(), now)
	requests := make([]refreshRequest, 0, len(g.state.Groups))
	seen := map[string]bool{}
	for _, group := range g.state.Groups {
		if group == nil || strings.TrimSpace(group.MainAuthID) == "" || seen[group.MainAuthID] {
			continue
		}
		account := g.ensureAccountByKeyLocked(group.MainAuthID)
		if len(account.QuotaSnapshots) > 0 && !quotaSnapshotsStale(account, now, g.cfg) {
			continue
		}
		seen[group.MainAuthID] = true
		requests = append(requests, refreshRequest{AuthID: account.AuthID, AuthIndex: account.AuthIndex})
	}
	return requests
}

func fetchKeeperUsageSnapshot(endpoint string, now time.Time) (keeperUsageSnapshot, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return keeperUsageSnapshot{}, fmt.Errorf("usage endpoint is required")
	}
	result, errCall := callHostFunc(pluginabi.MethodHostHTTPDo, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     endpoint,
		Headers: http.Header{"accept": []string{contentTypeJSON}},
	})
	if errCall != nil {
		return keeperUsageSnapshot{}, errCall
	}
	var resp pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return keeperUsageSnapshot{}, fmt.Errorf("decode keeper usage response: %w", errUnmarshal)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return keeperUsageSnapshot{}, fmt.Errorf("keeper usage returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(resp.Body)))
	}
	var payload keeperRealtimeUsageResponse
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		return keeperUsageSnapshot{}, fmt.Errorf("decode keeper usage payload: %w", errUnmarshal)
	}
	if payload.WindowEnd.IsZero() || now.Sub(payload.WindowEnd) > 5*time.Minute || payload.WindowEnd.After(now.Add(5*time.Minute)) {
		return keeperUsageSnapshot{}, fmt.Errorf("keeper usage snapshot is stale")
	}
	snapshot := keeperUsageSnapshot{WindowStart: payload.WindowStart, WindowEnd: payload.WindowEnd, FetchedAt: now, AuthFiles: map[string]keeperUsageItem{}}
	for _, item := range payload.CurrentUsage.AuthFiles {
		key := strings.TrimSpace(item.Key)
		if key == "" {
			continue
		}
		snapshot.AuthFiles[key] = keeperUsageItem{AuthIndex: key, Label: item.Label, Tokens: math.Max(0, item.Tokens), Requests: item.Requests, Share: item.Share}
	}
	return snapshot, nil
}

type bindingLoadEstimate struct {
	ClientID string
	Tokens   float64
	Idle     time.Duration
}

func (g *quotaGuard) analyzeRebalanceLocked(snapshot keeperUsageSnapshot, forceApply bool) rebalanceHistoryEntry {
	return g.analyzeRebalanceWindowsLocked(snapshot, snapshot, forceApply)
}

func (g *quotaGuard) analyzeRebalanceWindowsLocked(fastSnapshot, slowSnapshot keeperUsageSnapshot, forceApply bool) rebalanceHistoryEntry {
	now := g.now()
	g.ensureAffinityStateLocked()
	if g.state.Rebalance.OverloadStreak == nil {
		g.state.Rebalance.OverloadStreak = map[string]int{}
	}
	g.pruneClientActivityLocked(now)
	g.rebuildAffinityGroupsLocked(g.affinitySnapshotCandidatesLocked(), now)
	g.state.Rebalance.LastAnalysisAt = now
	g.state.Rebalance.KeeperFastUsage = fastSnapshot
	g.state.Rebalance.KeeperUsage = slowSnapshot
	g.state.Rebalance.LastError = ""
	loads, errLoads := g.buildCompositeGroupLoadsLocked(fastSnapshot, slowSnapshot, now)
	if errLoads != nil {
		return g.recordRebalanceFailureLocked(now, errLoads.Error())
	}
	g.state.Rebalance.Groups = loads
	totalTokens := 0.0
	for _, load := range loads {
		totalTokens += load.Tokens
	}
	if totalTokens <= 0 {
		return g.appendRebalanceHistoryLocked(rebalanceHistoryEntry{At: now, Action: "analyze", Result: "skipped", Reason: "no usage in analysis window"})
	}
	if now.Sub(g.state.Rebalance.StartedAt) < time.Duration(g.cfg.ClientAffinityRebalanceWarmupSecs)*time.Second {
		return g.appendRebalanceHistoryLocked(rebalanceHistoryEntry{At: now, Action: "analyze", Result: "observed", Reason: "warmup period is still active"})
	}
	streakReady := false
	for groupID, load := range loads {
		if load.Eligible && load.LoadFactor >= g.cfg.ClientAffinityRebalanceOverload {
			g.state.Rebalance.OverloadStreak[groupID]++
		} else {
			g.state.Rebalance.OverloadStreak[groupID] = 0
		}
		if g.state.Rebalance.OverloadStreak[groupID] >= g.cfg.ClientAffinityRebalanceStreak {
			streakReady = true
		}
	}
	if !streakReady {
		return g.appendRebalanceHistoryLocked(rebalanceHistoryEntry{At: now, Action: "analyze", Result: "observed", Reason: fmt.Sprintf("waiting for %d consecutive overload samples", g.cfg.ClientAffinityRebalanceStreak)})
	}
	candidate, target, improvement, ok := g.bestGlobalRebalanceMoveLocked(loads, fastSnapshot.WindowStart, slowSnapshot.WindowStart, now)
	if !ok {
		return g.appendRebalanceHistoryLocked(rebalanceHistoryEntry{At: now, Action: "analyze", Result: "deferred", Reason: "no safe client move improves global pressure"})
	}
	binding := g.state.ClientBindings[candidate.ClientID]
	if binding == nil {
		return g.appendRebalanceHistoryLocked(rebalanceHistoryEntry{At: now, Action: "analyze", Result: "skipped", ClientID: candidate.ClientID, Reason: "binding disappeared during analysis"})
	}
	source := loads[binding.GroupID]
	entry := rebalanceHistoryEntry{At: now, Action: "analyze", Result: "recommended", ClientID: candidate.ClientID, FromGroup: source.GroupID, ToGroup: target.GroupID, Reason: "capacity-normalized Keeper load is imbalanced", SourceTokens: source.Tokens, TargetTokens: target.Tokens, SourceLoadFactor: source.LoadFactor, TargetLoadFactor: target.LoadFactor, IdleSeconds: int64(candidate.Idle.Seconds()), EstimatedTokens: candidate.Tokens, ImprovementPercent: improvement}
	if improvement < g.cfg.ClientAffinityRebalanceMinImprove {
		entry.Result = "skipped"
		entry.Reason = fmt.Sprintf("predicted improvement %.2f%% is below %.2f%%", improvement, g.cfg.ClientAffinityRebalanceMinImprove)
		return g.appendRebalanceHistoryLocked(entry)
	}
	apply := forceApply || g.cfg.ClientAffinityRebalanceMode == "auto"
	if !apply {
		return g.appendRebalanceHistoryLocked(entry)
	}
	if binding == nil || binding.GroupID != source.GroupID {
		entry.Result = "skipped"
		entry.Reason = "binding changed during analysis"
		return g.appendRebalanceHistoryLocked(entry)
	}
	binding.GroupID = target.GroupID
	binding.UpdatedAt = now
	binding.LastAutoMoveAt = now
	binding.LastMoveReason = fmt.Sprintf("auto rebalance %.2f%% improvement from %s to %s", improvement, source.GroupID, target.GroupID)
	entry.Action = "auto"
	if forceApply {
		entry.Action = "manual-rebalance"
	}
	entry.Result = "moved"
	entry.Reason = binding.LastMoveReason
	return g.appendRebalanceHistoryLocked(entry)
}

func (g *quotaGuard) buildGroupLoadsLocked(snapshot keeperUsageSnapshot, now time.Time) (map[string]groupLoadState, error) {
	loads := map[string]groupLoadState{}
	authGroups := map[string][]string{}
	totalCapacity := 0.0
	for groupID, group := range g.state.Groups {
		if group == nil || strings.TrimSpace(group.MainAuthID) == "" {
			continue
		}
		main := g.ensureAccountByKeyLocked(group.MainAuthID)
		capacity := g.effectiveAffinityCapacityLocked(main, now)
		eligible, reason := g.affinityGroupEligibleFromStateLocked(groupID, now)
		loads[groupID] = groupLoadState{GroupID: groupID, Capacity: capacity, Eligible: eligible, Reason: reason}
		if eligible {
			totalCapacity += capacity
		}
		for _, member := range group.Members {
			account := g.ensureAccountByKeyLocked(member)
			if account.AuthIndex != "" {
				authGroups[account.AuthIndex] = append(authGroups[account.AuthIndex], groupID)
			}
		}
	}
	if totalCapacity <= 0 {
		return nil, fmt.Errorf("no eligible group capacity")
	}
	windowStart := snapshot.WindowStart
	for authIndex, usage := range snapshot.AuthFiles {
		groups := normalizeStringList(authGroups[authIndex])
		if len(groups) == 0 {
			continue
		}
		if len(groups) == 1 {
			load := loads[groups[0]]
			load.Tokens += usage.Tokens
			load.Requests += float64(usage.Requests)
			loads[groups[0]] = load
			continue
		}
		pickCounts := map[string]float64{}
		totalPicks := 0.0
		for _, event := range g.state.ClientActivity {
			if event.Kind != "pick" || event.AuthID == "" || event.At.Before(windowStart) {
				continue
			}
			account := g.ensureAccountByKeyLocked(event.AuthID)
			if account.AuthIndex == authIndex {
				pickCounts[event.GroupID]++
				totalPicks++
			}
		}
		if usage.Tokens > 0 && totalPicks == 0 {
			return nil, fmt.Errorf("shared auth %s usage cannot be attributed to groups", authIndex)
		}
		for _, groupID := range groups {
			share := 0.0
			if totalPicks > 0 {
				share = pickCounts[groupID] / totalPicks
			}
			load := loads[groupID]
			load.Tokens += usage.Tokens * share
			load.Requests += float64(usage.Requests) * share
			loads[groupID] = load
		}
	}
	totalTokens := 0.0
	for _, load := range loads {
		totalTokens += load.Tokens
	}
	for groupID, load := range loads {
		if load.Eligible {
			load.TargetShare = load.Capacity / totalCapacity * 100
		}
		if totalTokens > 0 {
			load.ActualShare = load.Tokens / totalTokens * 100
		}
		if load.TargetShare > 0 {
			load.LoadFactor = load.ActualShare / load.TargetShare
		}
		loads[groupID] = load
	}
	return loads, nil
}

func (g *quotaGuard) buildCompositeGroupLoadsLocked(fastSnapshot, slowSnapshot keeperUsageSnapshot, now time.Time) (map[string]groupLoadState, error) {
	fastLoads, errFast := g.buildGroupLoadsLocked(fastSnapshot, now)
	if errFast != nil {
		return nil, errFast
	}
	slowLoads, errSlow := g.buildGroupLoadsLocked(slowSnapshot, now)
	if errSlow != nil {
		return nil, errSlow
	}
	if fastSnapshot.WindowStart.Equal(slowSnapshot.WindowStart) {
		for id, load := range slowLoads {
			load.FastTokens = load.Tokens
			load.SlowTokens = load.Tokens
			load.EffectiveCapacity = load.Capacity
			slowLoads[id] = load
		}
		return slowLoads, nil
	}
	fastMinutes := fastSnapshot.WindowEnd.Sub(fastSnapshot.WindowStart).Minutes()
	slowMinutes := slowSnapshot.WindowEnd.Sub(slowSnapshot.WindowStart).Minutes()
	if fastMinutes <= 0 || slowMinutes <= 0 {
		return nil, fmt.Errorf("invalid Keeper usage windows")
	}
	fastWeight := g.cfg.ClientAffinityRebalanceFastWeight
	out := make(map[string]groupLoadState, len(slowLoads))
	totalTokens := 0.0
	for id, slow := range slowLoads {
		fast := fastLoads[id]
		predictedTokens := fastWeight*(fast.Tokens/fastMinutes*slowMinutes) + (1-fastWeight)*slow.Tokens
		predictedRequests := fastWeight*(fast.Requests/fastMinutes*slowMinutes) + (1-fastWeight)*slow.Requests
		slow.Tokens = math.Max(0, predictedTokens)
		slow.Requests = math.Max(0, predictedRequests)
		slow.FastTokens = fast.Tokens
		slow.SlowTokens = slowLoads[id].Tokens
		slow.EffectiveCapacity = slow.Capacity
		out[id] = slow
		totalTokens += slow.Tokens
	}
	totalCapacity := 0.0
	for _, load := range out {
		if load.Eligible {
			totalCapacity += load.Capacity
		}
	}
	for id, load := range out {
		if load.Eligible && totalCapacity > 0 {
			load.TargetShare = load.Capacity / totalCapacity * 100
		}
		if totalTokens > 0 {
			load.ActualShare = load.Tokens / totalTokens * 100
		}
		if load.TargetShare > 0 {
			load.LoadFactor = load.ActualShare / load.TargetShare
		}
		out[id] = load
	}
	return out, nil
}

func (g *quotaGuard) effectiveAffinityCapacityLocked(account *accountState, now time.Time) float64 {
	base := g.accountAffinityWeightLocked(account, now)
	if account == nil || base <= 0 {
		return 1
	}
	remaining, _ := g.remainingPercentLocked(account, now)
	reserve := g.cfg.MinRemainingPercent
	factor := (remaining - reserve) / math.Max(1, 100-reserve)
	factor = math.Max(0.1, math.Min(1, factor))
	return base * factor
}

func selectRebalanceGroups(loads map[string]groupLoadState) (groupLoadState, groupLoadState, bool) {
	var source, target groupLoadState
	set := false
	for _, load := range loads {
		if !load.Eligible || load.Capacity <= 0 {
			continue
		}
		if !set {
			source, target, set = load, load, true
			continue
		}
		if load.LoadFactor > source.LoadFactor || (load.LoadFactor == source.LoadFactor && load.GroupID < source.GroupID) {
			source = load
		}
		if load.LoadFactor < target.LoadFactor || (load.LoadFactor == target.LoadFactor && load.GroupID < target.GroupID) {
			target = load
		}
	}
	return source, target, set && source.GroupID != target.GroupID
}

func (g *quotaGuard) bestRebalanceBindingLocked(source, target groupLoadState, loads map[string]groupLoadState, windowStart, now time.Time) (bindingLoadEstimate, float64, bool) {
	idleThreshold := time.Duration(g.cfg.ClientAffinityRebalanceIdleSecs) * time.Second
	autoCooldown := time.Duration(g.cfg.ClientAffinityRebalanceCooldownSecs) * time.Second
	manualCooldown := time.Duration(g.cfg.ClientAffinityManualCooldownSecs) * time.Second
	type activityTotal struct{ score, picks float64 }
	clientTotals := map[string]activityTotal{}
	groupTotal := activityTotal{}
	for _, event := range g.state.ClientActivity {
		if event.GroupID != source.GroupID || event.At.Before(windowStart) {
			continue
		}
		total := clientTotals[event.ClientID]
		if event.Kind == "usage" {
			total.score += event.Score
			groupTotal.score += event.Score
		} else if event.Kind == "pick" {
			total.picks++
			groupTotal.picks++
		}
		clientTotals[event.ClientID] = total
	}
	beforeSpread := rebalanceLoadSpread(loads)
	bestImprove := -1.0
	best := bindingLoadEstimate{}
	for clientID, binding := range g.state.ClientBindings {
		if binding == nil || binding.GroupID != source.GroupID || binding.LastSeenAt.Before(windowStart) {
			continue
		}
		idle := now.Sub(binding.LastSeenAt)
		if idle < idleThreshold || (!binding.LastAutoMoveAt.IsZero() && now.Sub(binding.LastAutoMoveAt) < autoCooldown) || (!binding.LastManualMoveAt.IsZero() && now.Sub(binding.LastManualMoveAt) < manualCooldown) {
			continue
		}
		total := clientTotals[clientID]
		estimated := 0.0
		if groupTotal.score > 0 && total.score > 0 {
			estimated = source.Tokens * total.score / groupTotal.score
		} else if groupTotal.picks > 0 && total.picks > 0 {
			estimated = source.Tokens * total.picks / groupTotal.picks
		}
		if estimated <= 0 || estimated > source.Tokens {
			continue
		}
		simulated := make(map[string]groupLoadState, len(loads))
		for id, load := range loads {
			simulated[id] = load
		}
		sourceAfter := simulated[source.GroupID]
		targetAfter := simulated[target.GroupID]
		sourceAfter.Tokens -= estimated
		targetAfter.Tokens += estimated
		simulated[source.GroupID] = sourceAfter
		simulated[target.GroupID] = targetAfter
		totalTokens := 0.0
		for _, load := range simulated {
			totalTokens += load.Tokens
		}
		for id, load := range simulated {
			if totalTokens > 0 {
				load.ActualShare = load.Tokens / totalTokens * 100
			}
			if load.TargetShare > 0 {
				load.LoadFactor = load.ActualShare / load.TargetShare
			}
			simulated[id] = load
		}
		improvement := 0.0
		if beforeSpread > 0 {
			improvement = (beforeSpread - rebalanceLoadSpread(simulated)) / beforeSpread * 100
		}
		if improvement > bestImprove || (improvement == bestImprove && clientID < best.ClientID) {
			bestImprove = improvement
			best = bindingLoadEstimate{ClientID: clientID, Tokens: estimated, Idle: idle}
		}
	}
	return best, round2(bestImprove), bestImprove >= 0
}

func (g *quotaGuard) bestGlobalRebalanceMoveLocked(loads map[string]groupLoadState, fastStart, slowStart, now time.Time) (bindingLoadEstimate, groupLoadState, float64, bool) {
	quietPeriod := time.Duration(g.cfg.ClientAffinityRebalanceIdleSecs) * time.Second
	autoCooldown := time.Duration(g.cfg.ClientAffinityRebalanceCooldownSecs) * time.Second
	manualCooldown := time.Duration(g.cfg.ClientAffinityManualCooldownSecs) * time.Second
	fastMinutes := math.Max(1, now.Sub(fastStart).Minutes())
	slowMinutes := math.Max(fastMinutes, now.Sub(slowStart).Minutes())
	fastWeight := g.cfg.ClientAffinityRebalanceFastWeight
	type activityTotal struct{ fastScore, slowScore, fastPicks, slowPicks float64 }
	clientTotals := map[string]activityTotal{}
	groupTotals := map[string]activityTotal{}
	for _, event := range g.state.ClientActivity {
		if event.At.Before(slowStart) {
			continue
		}
		client := clientTotals[event.ClientID]
		group := groupTotals[event.GroupID]
		if event.Kind == "usage" {
			client.slowScore += event.Score
			group.slowScore += event.Score
			if !event.At.Before(fastStart) {
				client.fastScore += event.Score
				group.fastScore += event.Score
			}
		} else if event.Kind == "pick" {
			client.slowPicks++
			group.slowPicks++
			if !event.At.Before(fastStart) {
				client.fastPicks++
				group.fastPicks++
			}
		}
		clientTotals[event.ClientID] = client
		groupTotals[event.GroupID] = group
	}
	beforeMax := maxGroupPressure(loads)
	bestImprove := -1.0
	bestCandidate := bindingLoadEstimate{}
	bestTarget := groupLoadState{}
	for clientID, binding := range g.state.ClientBindings {
		if binding == nil || binding.LastSeenAt.Before(slowStart) {
			continue
		}
		if !binding.LastAutoMoveAt.IsZero() && now.Sub(binding.LastAutoMoveAt) < autoCooldown {
			continue
		}
		if !binding.LastManualMoveAt.IsZero() && now.Sub(binding.LastManualMoveAt) < manualCooldown {
			continue
		}
		source, okSource := loads[binding.GroupID]
		if !okSource || !source.Eligible || source.LoadFactor < g.cfg.ClientAffinityRebalanceOverload || g.state.Rebalance.OverloadStreak[source.GroupID] < g.cfg.ClientAffinityRebalanceStreak {
			continue
		}
		clientActivity := clientTotals[clientID]
		groupActivity := groupTotals[source.GroupID]
		clientWeight := compositeActivityWeight(clientActivity.fastScore, clientActivity.slowScore, fastMinutes, slowMinutes, fastWeight)
		groupWeight := compositeActivityWeight(groupActivity.fastScore, groupActivity.slowScore, fastMinutes, slowMinutes, fastWeight)
		if clientWeight <= 0 || groupWeight <= 0 {
			clientWeight = compositeActivityWeight(clientActivity.fastPicks, clientActivity.slowPicks, fastMinutes, slowMinutes, fastWeight)
			groupWeight = compositeActivityWeight(groupActivity.fastPicks, groupActivity.slowPicks, fastMinutes, slowMinutes, fastWeight)
		}
		if clientWeight <= 0 || groupWeight <= 0 {
			continue
		}
		estimated := source.Tokens * clientWeight / groupWeight
		if estimated <= 0 || estimated > source.Tokens {
			continue
		}
		for targetID, target := range loads {
			if targetID == source.GroupID || !target.Eligible || target.LoadFactor > g.cfg.ClientAffinityRebalanceTarget {
				continue
			}
			simulated := cloneGroupLoads(loads)
			sourceAfter := simulated[source.GroupID]
			targetAfter := simulated[targetID]
			sourceAfter.Tokens = math.Max(0, sourceAfter.Tokens-estimated)
			targetAfter.Tokens += estimated
			simulated[source.GroupID] = sourceAfter
			simulated[targetID] = targetAfter
			recomputeGroupLoadShares(simulated)
			afterMax := maxGroupPressure(simulated)
			rawImprovement := 0.0
			if beforeMax > 0 {
				rawImprovement = (beforeMax - afterMax) / beforeMax * 100
			}
			idle := now.Sub(binding.LastSeenAt)
			recencyPenalty := 0.0
			if quietPeriod > 0 && idle < quietPeriod {
				recencyPenalty = (1 - math.Max(0, idle.Seconds())/quietPeriod.Seconds()) * 5
			}
			inflightPenalty := math.Min(10, float64(g.clientInflightLocked(clientID))*2)
			improvement := rawImprovement - recencyPenalty - inflightPenalty
			if improvement > bestImprove || (improvement == bestImprove && (clientID < bestCandidate.ClientID || bestCandidate.ClientID == "")) {
				bestImprove = improvement
				bestCandidate = bindingLoadEstimate{ClientID: clientID, Tokens: estimated, Idle: idle}
				bestTarget = target
			}
		}
	}
	return bestCandidate, bestTarget, round2(bestImprove), bestImprove >= 0
}

func compositeActivityWeight(fast, slow, fastMinutes, slowMinutes, fastWeight float64) float64 {
	return fastWeight*(fast/fastMinutes*slowMinutes) + (1-fastWeight)*slow
}

func (g *quotaGuard) clientInflightLocked(clientID string) int {
	count := 0
	for _, account := range g.state.Accounts {
		if account == nil {
			continue
		}
		for _, reserve := range account.Inflight {
			if reserve.ClientID == clientID {
				count++
			}
		}
	}
	return count
}

func cloneGroupLoads(loads map[string]groupLoadState) map[string]groupLoadState {
	out := make(map[string]groupLoadState, len(loads))
	for id, load := range loads {
		out[id] = load
	}
	return out
}

func recomputeGroupLoadShares(loads map[string]groupLoadState) {
	totalTokens := 0.0
	totalCapacity := 0.0
	for _, load := range loads {
		totalTokens += load.Tokens
		if load.Eligible {
			totalCapacity += load.Capacity
		}
	}
	for id, load := range loads {
		if load.Eligible && totalCapacity > 0 {
			load.TargetShare = load.Capacity / totalCapacity * 100
		}
		if totalTokens > 0 {
			load.ActualShare = load.Tokens / totalTokens * 100
		}
		if load.TargetShare > 0 {
			load.LoadFactor = load.ActualShare / load.TargetShare
		}
		loads[id] = load
	}
}

func maxGroupPressure(loads map[string]groupLoadState) float64 {
	maxPressure := 0.0
	for _, load := range loads {
		if load.Eligible {
			maxPressure = math.Max(maxPressure, load.LoadFactor)
		}
	}
	return maxPressure
}

func rebalanceLoadSpread(loads map[string]groupLoadState) float64 {
	minLoad := math.Inf(1)
	maxLoad := 0.0
	for _, load := range loads {
		if !load.Eligible {
			continue
		}
		minLoad = math.Min(minLoad, load.LoadFactor)
		maxLoad = math.Max(maxLoad, load.LoadFactor)
	}
	if math.IsInf(minLoad, 1) {
		return 0
	}
	return maxLoad - minLoad
}

func (g *quotaGuard) recordRebalanceFailureLocked(now time.Time, reason string) rebalanceHistoryEntry {
	g.state.Rebalance.LastAnalysisAt = now
	g.state.Rebalance.LastError = reason
	return g.appendRebalanceHistoryLocked(rebalanceHistoryEntry{At: now, Action: "analyze", Result: "error", Reason: reason})
}

func (g *quotaGuard) appendRebalanceHistoryLocked(entry rebalanceHistoryEntry) rebalanceHistoryEntry {
	g.state.Rebalance.History = append(g.state.Rebalance.History, entry)
	limit := g.cfg.ClientAffinityRebalanceHistoryLimit
	if limit <= 0 {
		limit = 200
	}
	if len(g.state.Rebalance.History) > limit {
		g.state.Rebalance.History = append([]rebalanceHistoryEntry(nil), g.state.Rebalance.History[len(g.state.Rebalance.History)-limit:]...)
	}
	return entry
}

func (g *quotaGuard) deleteLocalAccountState(authID, authIndex string, files []pluginapi.HostAuthFileEntry) error {
	authID = strings.TrimSpace(authID)
	authIndex = strings.TrimSpace(authIndex)
	if authID == "" && authIndex == "" {
		return fmt.Errorf("auth_id or auth_index is required")
	}
	matches := hostAuthMatchSet(files)
	if matches.IDs[authID] || matches.Indexes[authIndex] {
		return fmt.Errorf("refusing to delete state for an active host auth")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var deletedKey string
	for key, account := range g.state.Accounts {
		if account == nil {
			continue
		}
		if authID != "" && (key == authID || account.AuthID == authID) {
			deletedKey = key
			break
		}
		if authIndex != "" && (key == authIndex || account.AuthIndex == authIndex) {
			deletedKey = key
			break
		}
	}
	if deletedKey == "" {
		return fmt.Errorf("local account state not found")
	}
	deleted := g.state.Accounts[deletedKey]
	delete(g.state.Accounts, deletedKey)
	if deleted != nil && (g.state.CurrentAuthID == deleted.AuthID || g.state.CurrentAuthIndex == deleted.AuthIndex || g.state.CurrentAuthID == deletedKey) {
		g.state.CurrentAuthID = ""
		g.state.CurrentAuthIndex = ""
		g.state.CurrentRole = ""
		g.state.LastSelectedAt = time.Time{}
	}
	g.saveErr = g.saveStateLocked()
	return g.saveErr
}

func resourceCalibrateTargetExists(req calibrateRequest, files []pluginapi.HostAuthFileEntry) bool {
	authID := strings.TrimSpace(req.AuthID)
	authIndex := strings.TrimSpace(req.AuthIndex)
	if authID == "" && authIndex == "" {
		return false
	}
	for _, file := range files {
		if authID != "" && strings.TrimSpace(file.ID) == authID {
			return true
		}
		if authIndex != "" && strings.TrimSpace(file.AuthIndex) == authIndex {
			return true
		}
	}
	return false
}

func normalizeManagementPath(path string) string {
	path = strings.TrimSuffix(strings.TrimSpace(path), "/")
	switch {
	case strings.HasPrefix(path, "/v0/resource/plugins/quota-guard"):
		path = strings.TrimPrefix(path, "/v0/resource/plugins/quota-guard")
	case strings.HasPrefix(path, "/v0/management"):
		path = strings.TrimPrefix(path, "/v0/management")
	}
	if path == "" {
		return "/"
	}
	return path
}

func decodeCalibrateRequest(raw []byte) (calibrateRequest, error) {
	var body calibrateRequest
	if len(bytes.TrimSpace(raw)) == 0 {
		return body, fmt.Errorf("request body is required")
	}
	if json.Valid(raw) {
		var payload map[string]any
		if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
			return body, errUnmarshal
		}
		body.AuthID = stringFromAny(payload["auth_id"])
		body.AuthIndex = stringFromAny(payload["auth_index"])
		body.Window = stringFromAny(payload["window"])
		body.Source = stringFromAny(payload["source"])
		if percent, ok := numberValue(payload["remaining_percent"]); ok {
			body.RemainingPercent = &percent
		}
		if percent, ok := numberValue(payload["actual_remaining_percent"]); ok {
			body.ActualRemainingPercent = &percent
		}
		return body, nil
	}
	values, errParse := url.ParseQuery(string(raw))
	if errParse != nil {
		return body, errParse
	}
	body.AuthID = values.Get("auth_id")
	body.AuthIndex = values.Get("auth_index")
	body.Window = values.Get("window")
	body.Source = values.Get("source")
	if rawPercent := strings.TrimSpace(values.Get("remaining_percent")); rawPercent != "" {
		percent, errPercent := strconv.ParseFloat(rawPercent, 64)
		if errPercent != nil {
			return body, errPercent
		}
		body.RemainingPercent = &percent
	}
	return body, nil
}

func stringFromAny(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func decodeResetWindowRequest(raw []byte) (resetWindowRequest, error) {
	var body resetWindowRequest
	if len(bytes.TrimSpace(raw)) == 0 {
		return body, fmt.Errorf("request body is required")
	}
	if json.Valid(raw) {
		return body, json.Unmarshal(raw, &body)
	}
	values, errParse := url.ParseQuery(string(raw))
	if errParse != nil {
		return body, errParse
	}
	body.AuthID = values.Get("auth_id")
	body.AuthIndex = values.Get("auth_index")
	body.Window = values.Get("window")
	return body, nil
}

func (g *quotaGuard) handleConfigPatch(raw []byte) ([]byte, error) {
	var patch map[string]any
	if errDecode := json.Unmarshal(raw, &patch); errDecode != nil {
		return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errDecode.Error()}))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cfg := g.cfg
	for key, value := range patch {
		switch key {
		case "min_remaining_percent":
			if v, ok := numberValue(value); ok {
				cfg.MinRemainingPercent = v
			}
		case "fail_when_all_low":
			if v, ok := value.(bool); ok {
				cfg.FailWhenAllLow = v
			}
		case "delegate_when_unconfigured":
			if v, ok := value.(string); ok {
				cfg.DelegateWhenUnconfigured = v
			}
		case "quota_refresh_enabled":
			if v, ok := value.(bool); ok {
				cfg.QuotaRefreshEnabled = v
			}
		case "quota_refresh_endpoint":
			if v, ok := value.(string); ok {
				cfg.QuotaRefreshEndpoint = v
			}
		case "quota_refresh_interval_seconds":
			if v, ok := numberValue(value); ok {
				cfg.QuotaRefreshIntervalSecs = int64(v)
			}
		case "quota_refresh_min_interval_per_auth_seconds":
			if v, ok := numberValue(value); ok {
				cfg.QuotaRefreshMinIntervalSecs = int64(v)
			}
		case "quota_refresh_timeout_seconds":
			if v, ok := numberValue(value); ok {
				cfg.QuotaRefreshTimeoutSecs = int64(v)
			}
		case "quota_refresh_on_startup":
			if v, ok := value.(bool); ok {
				cfg.QuotaRefreshOnStartup = v
			}
		case "quota_snapshot_max_age_seconds":
			if v, ok := numberValue(value); ok {
				cfg.QuotaSnapshotMaxAgeSecs = int64(v)
			}
		case "resource_actions_require_management_key":
			if v, ok := value.(bool); ok {
				cfg.ResourceActionsRequireManagementKey = v
			}
		case "client_affinity_rebalance_enabled":
			if v, ok := value.(bool); ok {
				cfg.ClientAffinityRebalanceEnabled = v
			}
		case "client_affinity_rebalance_mode":
			if v, ok := value.(string); ok {
				cfg.ClientAffinityRebalanceMode = v
			}
		case "client_affinity_rebalance_usage_endpoint":
			if v, ok := value.(string); ok {
				cfg.ClientAffinityRebalanceUsageURL = v
			}
		case "client_affinity_rebalance_interval_seconds":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceIntervalSecs = int64(v)
			}
		case "client_affinity_rebalance_window_minutes":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceWindowMins = int64(v)
			}
		case "client_affinity_rebalance_idle_seconds":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceIdleSecs = int64(v)
			}
		case "client_affinity_rebalance_cooldown_seconds":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceCooldownSecs = int64(v)
			}
		case "client_affinity_manual_move_cooldown_seconds":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityManualCooldownSecs = int64(v)
			}
		case "client_affinity_rebalance_warmup_seconds":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceWarmupSecs = int64(v)
			}
		case "client_affinity_rebalance_max_moves_per_cycle":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceMaxMoves = int(v)
			}
		case "client_affinity_rebalance_min_load_ratio":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceMinLoadRatio = v
			}
		case "client_affinity_rebalance_min_improvement_percent":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceMinImprove = v
			}
		case "client_affinity_rebalance_history_limit":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceHistoryLimit = int(v)
			}
		case "client_affinity_rebalance_fast_window_minutes":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceFastWindowMins = int64(v)
			}
		case "client_affinity_rebalance_fast_weight":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceFastWeight = v
			}
		case "client_affinity_rebalance_overload_threshold":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceOverload = v
			}
		case "client_affinity_rebalance_target_threshold":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceTarget = v
			}
		case "client_affinity_rebalance_overload_consecutive":
			if v, ok := numberValue(value); ok {
				cfg.ClientAffinityRebalanceStreak = int(v)
			}
		}
	}
	g.cfg = normalizeConfig(cfg)
	g.restartBackgroundRefreshLocked()
	return okEnvelope(jsonResponse(http.StatusOK, g.cfg))
}

func (g *quotaGuard) handleRefresh(raw []byte) ([]byte, error) {
	body, errDecode := decodeRefreshRequest(raw)
	if errDecode != nil {
		return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errDecode.Error()}))
	}
	auths, errAuths := callHostAuthList()
	if errAuths != nil {
		return okEnvelope(jsonResponse(http.StatusBadGateway, map[string]string{"error": errAuths.Error()}))
	}
	results := g.refreshQuotaSnapshots(auths.Files, body)
	status := "ok"
	for _, result := range results {
		if result.Error != "" {
			status = "partial"
			break
		}
	}
	return okEnvelope(jsonResponse(http.StatusOK, refreshResponse{
		Status:    status,
		Refreshed: results,
		Snapshot:  g.snapshot(true),
	}))
}

func decodeRefreshRequest(raw []byte) (refreshRequest, error) {
	var body refreshRequest
	if len(bytes.TrimSpace(raw)) == 0 {
		body.All = true
		return body, nil
	}
	if json.Valid(raw) {
		if errUnmarshal := json.Unmarshal(raw, &body); errUnmarshal != nil {
			return body, errUnmarshal
		}
		if !body.All && strings.TrimSpace(body.AuthID) == "" && strings.TrimSpace(body.AuthIndex) == "" {
			body.All = true
		}
		return body, nil
	}
	values, errParse := url.ParseQuery(string(raw))
	if errParse != nil {
		return body, errParse
	}
	body.AuthID = values.Get("auth_id")
	body.AuthIndex = values.Get("auth_index")
	body.All = values.Get("all") == "true" || (body.AuthID == "" && body.AuthIndex == "")
	body.Force = values.Get("force") == "true"
	return body, nil
}

func (g *quotaGuard) refreshQuotaSnapshots(files []pluginapi.HostAuthFileEntry, req refreshRequest) []refreshResult {
	g.ingestHostAuths(files, false)
	targets := make([]pluginapi.HostAuthFileEntry, 0, len(files))
	for _, file := range files {
		if !isCodexAuth(file) {
			continue
		}
		if !req.All && strings.TrimSpace(req.AuthID) != "" && strings.TrimSpace(file.ID) != strings.TrimSpace(req.AuthID) {
			continue
		}
		if !req.All && strings.TrimSpace(req.AuthIndex) != "" && strings.TrimSpace(file.AuthIndex) != strings.TrimSpace(req.AuthIndex) {
			continue
		}
		targets = append(targets, file)
	}
	results := make([]refreshResult, 0, len(targets))
	for _, file := range targets {
		results = append(results, g.refreshQuotaSnapshot(file, req.Force, req.AuthJSONOnly))
	}
	return results
}

func (g *quotaGuard) refreshQuotaSnapshot(file pluginapi.HostAuthFileEntry, force bool, authJSONOnly bool) refreshResult {
	result := refreshResult{AuthID: file.ID, AuthIndex: file.AuthIndex, Provider: firstNonEmpty(file.Provider, file.Type)}
	if strings.TrimSpace(file.AuthIndex) == "" {
		result.Error = "auth_index is required for quota refresh"
		g.recordQuotaRefreshResult(file, result)
		return result
	}
	g.mu.Lock()
	cfg := g.cfg
	key := strings.TrimSpace(file.ID)
	if key == "" {
		key = strings.TrimSpace(file.AuthIndex)
	}
	account := g.ensureAccountByKeyLocked(key)
	applyHostAuthFile(account, file)
	lastRefresh := account.LastQuotaRefreshAt
	g.mu.Unlock()
	if authJSONOnly || !cfg.QuotaRefreshEnabled {
		raw, errGet := callHostAuthGet(file.AuthIndex)
		if errGet != nil {
			result.Error = errGet.Error()
			g.recordQuotaRefreshResult(file, result)
			return result
		}
		return g.applyAuthJSONQuota(file, raw.JSON, "auth_json")
	}
	if !force && !lastRefresh.IsZero() && g.now().Sub(lastRefresh) < time.Duration(cfg.QuotaRefreshMinIntervalSecs)*time.Second {
		result.Skipped = true
		result.Source = "rate_limited"
		return result
	}
	if triggerEndpoint := strings.TrimSpace(cfg.QuotaRefreshTriggerEndpoint); triggerEndpoint != "" {
		if errTrigger := triggerQuotaRefresh(triggerEndpoint, file); errTrigger != nil {
			result.Error = errTrigger.Error()
			g.recordQuotaRefreshResult(file, result)
			return result
		}
		if cfg.QuotaRefreshTriggerWaitSecs > 0 {
			time.Sleep(time.Duration(cfg.QuotaRefreshTriggerWaitSecs) * time.Second)
		}
	}
	if endpoint := strings.TrimSpace(cfg.QuotaRefreshEndpoint); endpoint != "" {
		refreshBody, errRefresh := refreshThroughEndpoint(endpoint, file)
		if errRefresh != nil {
			result.Error = errRefresh.Error()
			if raw, errGet := callHostAuthGet(file.AuthIndex); errGet == nil {
				cached := g.applyAuthJSONQuota(file, raw.JSON, "auth_json")
				cached.Error = result.Error
				g.recordQuotaRefreshResult(file, cached)
				return cached
			}
			g.recordQuotaRefreshResult(file, result)
			return result
		}
		if refreshed, ok := g.applyKeeperQuotaRefresh(file, refreshBody, "keeper-refresh"); ok {
			return refreshed
		}
	}
	raw, errGet := callHostAuthGet(file.AuthIndex)
	if errGet != nil {
		result.Error = errGet.Error()
		g.recordQuotaRefreshResult(file, result)
		return result
	}
	return g.applyAuthJSONQuota(file, raw.JSON, "cpa-refresh")
}

func triggerQuotaRefresh(endpoint string, file pluginapi.HostAuthFileEntry) error {
	urlValue := strings.NewReplacer(
		"{auth_index}", url.QueryEscape(file.AuthIndex),
		"{auth_id}", url.QueryEscape(file.ID),
		"{provider}", url.QueryEscape(firstNonEmpty(file.Provider, file.Type)),
	).Replace(endpoint)
	body, errMarshal := json.Marshal(map[string]any{"auth_indexes": []string{file.AuthIndex}})
	if errMarshal != nil {
		return errMarshal
	}
	result, errCall := callHostFunc(pluginabi.MethodHostHTTPDo, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    urlValue,
		Headers: http.Header{
			"accept":                     []string{contentTypeJSON},
			"content-type":               []string{contentTypeJSON},
			"X-Cpa-Usage-Keeper-Request": []string{"fetch"},
		},
		Body: body,
	})
	if errCall != nil {
		return errCall
	}
	var resp pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return fmt.Errorf("decode refresh trigger response: %w", errUnmarshal)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("quota refresh trigger returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(resp.Body)))
	}
	return nil
}

func refreshThroughEndpoint(endpoint string, file pluginapi.HostAuthFileEntry) ([]byte, error) {
	urlValue := strings.NewReplacer(
		"{auth_index}", url.QueryEscape(file.AuthIndex),
		"{auth_id}", url.QueryEscape(file.ID),
		"{provider}", url.QueryEscape(firstNonEmpty(file.Provider, file.Type)),
	).Replace(endpoint)
	result, errCall := callHostFunc(pluginabi.MethodHostHTTPDo, pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    urlValue,
		Headers: http.Header{
			"accept": []string{contentTypeJSON},
		},
	})
	if errCall != nil {
		return nil, errCall
	}
	var resp pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode refresh response: %w", errUnmarshal)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("quota refresh returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(resp.Body)))
	}
	return resp.Body, nil
}

func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		parsed, errParse := strconv.ParseFloat(v, 64)
		return parsed, errParse == nil
	default:
		return 0, false
	}
}

func (g *quotaGuard) handleQueryAndCalibrate(raw []byte) ([]byte, error) {
	var body queryCalibrateRequest
	if errDecode := json.Unmarshal(raw, &body); errDecode != nil {
		return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errDecode.Error()}))
	}
	g.mu.Lock()
	endpoint := strings.TrimSpace(g.cfg.QuotaQueryURL)
	minInterval := time.Duration(g.cfg.QuotaQueryMinIntervalSecs) * time.Second
	key := strings.TrimSpace(body.AuthID)
	if key == "" {
		key = strings.TrimSpace(body.AuthIndex)
	}
	window, errWindow := normalizeWindow(body.Window)
	var account *accountState
	if key != "" {
		account = g.ensureAccountByKeyLocked(key)
	}
	if errWindow == nil {
		var last time.Time
		if account != nil {
			last = account.LastQueryAt[window]
		}
		if !last.IsZero() && g.now().Sub(last) < minInterval {
			errWindow = fmt.Errorf("quota query for %s/%s is rate limited", key, window)
		}
	}
	g.mu.Unlock()
	if endpoint == "" {
		return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": "quota_query_url is not configured"}))
	}
	if key == "" {
		return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": "auth_id or auth_index is required"}))
	}
	if errWindow != nil {
		return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errWindow.Error()}))
	}
	resp, errQuery := queryQuota(endpoint, body, window)
	if errQuery != nil {
		return okEnvelope(jsonResponse(http.StatusBadGateway, map[string]string{"error": errQuery.Error()}))
	}
	if resp.RemainingPercent == nil {
		return okEnvelope(jsonResponse(http.StatusBadGateway, map[string]string{"error": "quota query response missing remaining_percent"}))
	}
	if errCal := g.calibrate(calibrateRequest{AuthID: firstNonEmpty(resp.AuthID, body.AuthID), AuthIndex: firstNonEmpty(resp.AuthIndex, body.AuthIndex), Window: firstNonEmpty(resp.Window, window), RemainingPercent: resp.RemainingPercent, Source: "quota_query"}); errCal != nil {
		return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": errCal.Error()}))
	}
	g.mu.Lock()
	g.ensureAccountByKeyLocked(key).LastQueryAt[window] = g.now()
	g.saveErr = g.saveStateLocked()
	g.mu.Unlock()
	return okEnvelope(jsonResponse(http.StatusOK, g.snapshot(false)))
}

func queryQuota(endpoint string, req queryCalibrateRequest, window string) (quotaQueryResponse, error) {
	payload := map[string]string{"auth_id": req.AuthID, "auth_index": req.AuthIndex, "window": window}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return quotaQueryResponse{}, errMarshal
	}
	client := http.Client{Timeout: 15 * time.Second}
	httpResp, errPost := client.Post(endpoint, contentTypeJSON, bytes.NewReader(raw))
	if errPost != nil {
		return quotaQueryResponse{}, errPost
	}
	defer func() { _ = httpResp.Body.Close() }()
	var resp quotaQueryResponse
	if errDecode := json.NewDecoder(httpResp.Body).Decode(&resp); errDecode != nil {
		return quotaQueryResponse{}, errDecode
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return quotaQueryResponse{}, fmt.Errorf("quota query returned HTTP %d", httpResp.StatusCode)
	}
	return resp, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func jsonResponse(statusCode int, body any) managementResponse {
	raw, _ := json.MarshalIndent(body, "", "  ")
	return managementResponse{StatusCode: statusCode, Headers: http.Header{"content-type": []string{contentTypeJSON}}, Body: raw}
}

func htmlResponse(statusCode int, body []byte) managementResponse {
	return managementResponse{StatusCode: statusCode, Headers: http.Header{"content-type": []string{contentTypeHTML}}, Body: body}
}

const quotaGuardThemeScript = `<script>
(function(){
  try {
    var raw = localStorage.getItem("cli-proxy-theme");
    var theme = "auto";
    if (raw) {
      var parsed = JSON.parse(raw);
      theme = parsed && parsed.state && parsed.state.theme || theme;
    }
    var dark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
    var resolved = theme === "auto" ? (dark ? "dark" : "white") : theme;
    if (resolved === "dark" || resolved === "white") {
      document.documentElement.setAttribute("data-theme", resolved);
    }
  } catch (_) {}
})();
</script>`

const quotaGuardStatusStyle = `<style>
:root{--bg-secondary:#faf9f5;--bg-primary:#f0eee8;--bg-tertiary:#e9e6df;--bg-hover:var(--bg-tertiary);--bg-quinary:#f6f4ee;--text-primary:#2d2a26;--text-secondary:#6d6760;--text-tertiary:#a29c95;--text-muted:var(--text-tertiary);--border-color:#e3e1db;--border-primary:#d5d2cb;--border-hover:#cecac4;--primary-color:#8b8680;--primary-hover:#7f7a74;--primary-active:#726d67;--primary-contrast:#fff;--success-color:#10b981;--quota-medium-color:#e0aa14;--warning-color:#c65746;--error-color:#c65746;--success-badge-bg:#d1fae5;--success-badge-text:#065f46;--success-badge-border:#6ee7b7;--failure-badge-bg:#c6574624;--failure-badge-text:#8a3a30;--failure-badge-border:#c6574659;--count-badge-bg:#8b86802e;--count-badge-text:var(--primary-active);--shadow:0 1px 2px 0 #00000014;--shadow-lg:0 10px 18px -3px #0000001a;--radius-md:8px;--muted-bg:var(--bg-tertiary)}
[data-theme=white]{--bg-secondary:#fff;--bg-primary:#fff;--bg-tertiary:#f6f6f6;--bg-hover:var(--bg-tertiary);--bg-quinary:#fff;--text-primary:#2d2a26;--text-secondary:#6d6760;--text-tertiary:#a29c95;--text-muted:var(--text-tertiary);--border-color:#e5e5e5;--border-primary:#d9d9d9;--border-hover:#ccc;--primary-color:#8b8680;--primary-hover:#7f7a74;--primary-active:#726d67;--primary-contrast:#fff;--success-color:#10b981;--quota-medium-color:#e0aa14;--warning-color:#c65746;--error-color:#c65746;--success-badge-bg:#d1fae5;--success-badge-text:#065f46;--success-badge-border:#6ee7b7;--failure-badge-bg:#c6574624;--failure-badge-text:#8a3a30;--failure-badge-border:#c6574659;--count-badge-bg:#8b86802e;--count-badge-text:var(--primary-active);--shadow:0 1px 2px 0 #00000014;--shadow-lg:0 10px 18px -3px #0000001a;--radius-md:8px;--muted-bg:var(--bg-tertiary)}
[data-theme=dark]{--bg-secondary:#151412;--bg-primary:#1d1b18;--bg-tertiary:#262320;--bg-hover:#2e2a26;--bg-quinary:#191714;--text-primary:#f6f4f1;--text-secondary:#c9c3bb;--text-tertiary:#9c958d;--text-muted:var(--text-tertiary);--border-color:#3a3530;--border-primary:#4a453f;--border-hover:#5a544d;--primary-color:#8b8680;--primary-hover:#9a948e;--primary-active:#a6a099;--primary-contrast:#fff;--success-color:#10b981;--quota-medium-color:#ffd862;--warning-color:#c65746;--error-color:#c65746;--success-badge-bg:#064e3b4d;--success-badge-text:#6ee7b7;--success-badge-border:#059669;--failure-badge-bg:#c657463d;--failure-badge-text:#f1b0a6;--failure-badge-border:#c6574680;--count-badge-bg:#8b868047;--count-badge-text:var(--primary-active);--shadow:0 1px 3px 0 #0000004d;--shadow-lg:0 10px 15px -3px #0000004d;--radius-md:8px;--muted-bg:var(--bg-tertiary)}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:24px;color:var(--text-primary);background:var(--bg-primary)}main{max-width:1440px;margin:auto}h1,h2{color:var(--text-primary)}table{width:100%;border-collapse:separate;border-spacing:0;background:var(--bg-secondary);border:1px solid var(--border-color);border-radius:8px;table-layout:auto;overflow:hidden;box-shadow:var(--shadow)}th,td{padding:10px;border-bottom:1px solid var(--border-color);text-align:left;font-size:13px;vertical-align:top}tr:last-child td{border-bottom:0}th{background:var(--bg-tertiary);color:var(--text-secondary);font-weight:600}code,pre{background:var(--bg-tertiary);color:var(--text-primary);border:1px solid var(--border-color);border-radius:4px;padding:2px 4px}pre{white-space:pre-wrap;margin:0}.toolbar,.manual{display:flex;gap:8px;flex-wrap:wrap;margin:16px 0}.section{background:var(--bg-secondary);border:1px solid var(--border-color);border-radius:12px;box-shadow:var(--shadow);margin:16px 0;padding:12px}input,select,button{font:inherit;padding:8px;border:1px solid var(--border-color);border-radius:8px;background:var(--bg-secondary);color:var(--text-primary)}button{background:var(--primary-color);color:var(--primary-contrast);border-color:var(--primary-color);cursor:pointer;font-weight:600}button:hover{background:var(--primary-hover);border-color:var(--primary-hover)}button.secondary{background:var(--bg-tertiary);border-color:var(--border-color);color:var(--text-primary)}button.secondary:hover{background:var(--bg-hover);border-color:var(--border-hover)}.low{color:var(--error-color);font-weight:600}.ok{color:var(--success-color);font-weight:600}.muted{color:var(--text-secondary)}.pill{display:inline-block;border-radius:999px;padding:2px 8px;font-size:12px;background:var(--count-badge-bg);color:var(--count-badge-text);border:1px solid var(--border-color);margin-right:4px}.primary{background:var(--primary-color);color:var(--primary-contrast);border-color:var(--primary-color)}.bad{background:var(--failure-badge-bg);color:var(--failure-badge-text);border-color:var(--failure-badge-border)}.good{background:var(--success-badge-bg);color:var(--success-badge-text);border-color:var(--success-badge-border)}.quota-lines{min-width:170px}.quota-line{display:flex;justify-content:space-between;gap:12px;white-space:nowrap}.quota-label{font-weight:600}.tight{max-width:180px}.nowrap{white-space:nowrap}
</style>`

func renderStatusPage(status statusResponse) []byte {
	var out bytes.Buffer
	out.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>Quota Guard</title>")
	out.WriteString(quotaGuardThemeScript)
	out.WriteString(quotaGuardStatusStyle)
	out.WriteString("</head><body><main><h1>Quota Guard</h1>")
	out.WriteString("<p class=\"muted\">State file: <code>")
	out.WriteString(html.EscapeString(status.StateFile))
	out.WriteString("</code></p>")
	out.WriteString("<p>Current: ")
	if status.CurrentAuthID != "" {
		pillClass := "pill primary"
		if status.CurrentRole != "" && status.CurrentRole != "primary" {
			pillClass = "pill bad"
		}
		out.WriteString("<span class=\"")
		out.WriteString(pillClass)
		out.WriteString("\">")
		out.WriteString(html.EscapeString(firstNonEmpty(status.CurrentRole, "primary")))
		out.WriteString("</span><code>")
		out.WriteString(html.EscapeString(status.CurrentAuthID))
		out.WriteString("</code>")
		if status.CurrentAuthIndex != "" {
			out.WriteString(" <span class=\"muted\">")
			out.WriteString(html.EscapeString(status.CurrentAuthIndex))
			out.WriteString("</span>")
		}
		if status.CurrentRole != "primary" {
			out.WriteString(" <span class=\"muted\">next pick will reselect")
			if status.CurrentReason != "" {
				out.WriteString(": ")
				out.WriteString(html.EscapeString(status.CurrentReason))
			}
			out.WriteString("</span>")
		}
	} else {
		out.WriteString("<span class=\"muted\">none yet</span>")
	}
	out.WriteString("</p>")
	if status.LoadError != "" || status.SaveError != "" {
		out.WriteString("<pre>")
		out.WriteString(html.EscapeString(strings.TrimSpace(status.LoadError + "\n" + status.SaveError)))
		out.WriteString("</pre>")
	}
	renderAffinitySection(&out, status.Affinity, status.Accounts)
	out.WriteString("<div class=\"toolbar\"><button id=\"quota-guard-refresh-all\">Refresh All</button><button class=\"secondary\" onclick=\"location.reload()\">Reload</button><span id=\"quota-guard-message\" class=\"muted\"></span></div>")
	out.WriteString("<table><thead><tr><th>Role</th><th>Auth</th><th>Host status</th><th>Eligibility</th><th>Quota</th><th>Inflight</th><th>Recent</th><th>Refresh</th><th>Actions</th></tr></thead><tbody>")
	for _, account := range status.Accounts {
		className := "ok"
		if account.RemainingPercent < status.Config.MinRemainingPercent {
			className = "low"
		}
		out.WriteString("<tr><td>")
		if account.Role != "" {
			roleClass := "pill"
			if account.Role == "primary" {
				roleClass = "pill primary"
			}
			out.WriteString("<span class=\"")
			out.WriteString(roleClass)
			out.WriteString("\">")
			out.WriteString(html.EscapeString(account.Role))
			out.WriteString("</span>")
		}
		out.WriteString("</td><td><code>")
		out.WriteString(html.EscapeString(account.AuthID))
		out.WriteString("</code>")
		if account.AuthIndex != "" {
			out.WriteString("<br><span class=\"muted\">")
			out.WriteString(html.EscapeString(account.AuthIndex))
			out.WriteString("</span>")
		}
		out.WriteString("<br><span class=\"muted\">")
		out.WriteString(html.EscapeString(firstNonEmpty(account.Provider, "unknown")))
		out.WriteString(" priority ")
		out.WriteString(strconv.Itoa(account.Priority))
		if len(account.AffinityGroups) > 0 {
			out.WriteString("<br><span class=\"muted\">groups ")
			out.WriteString(html.EscapeString(strings.Join(account.AffinityGroups, ", ")))
			out.WriteString("</span>")
		}
		out.WriteString("</span></td><td>")
		out.WriteString(html.EscapeString(firstNonEmpty(account.Status, "unknown")))
		if account.Disabled {
			out.WriteString(" <span class=\"pill bad\">disabled</span>")
		}
		if account.Unavailable {
			out.WriteString(" <span class=\"pill bad\">unavailable</span>")
		}
		if !account.NextRetryAfter.IsZero() {
			out.WriteString("<br><span class=\"muted\">retry after ")
			out.WriteString(html.EscapeString(account.NextRetryAfter.Format("01-02 15:04")))
			out.WriteString("</span>")
		}
		if account.StatusMessage != "" {
			out.WriteString("<br><span class=\"muted\">")
			out.WriteString(html.EscapeString(account.StatusMessage))
			out.WriteString("</span>")
		}
		if account.UnknownStatusSeen > 0 {
			out.WriteString("<br><span class=\"muted\">unknown status seen ")
			out.WriteString(strconv.FormatInt(account.UnknownStatusSeen, 10))
			if !account.LastUnknownStatusAt.IsZero() {
				out.WriteString(" · last ")
				out.WriteString(html.EscapeString(account.LastUnknownStatusAt.Format("01-02 15:04")))
			}
			out.WriteString("</span>")
		}
		out.WriteString("</td><td>")
		if account.Eligible {
			out.WriteString("<span class=\"pill good\">eligible</span>")
		} else {
			out.WriteString("<span class=\"pill bad\">skipped</span>")
		}
		if account.Reason != "" {
			out.WriteString("<br><span class=\"muted\">")
			out.WriteString(html.EscapeString(account.Reason))
			out.WriteString("</span>")
		}
		out.WriteString("</td><td><span class=\"")
		out.WriteString(className)
		out.WriteString("\">")
		out.WriteString(fmt.Sprintf("%.2f%%", account.RemainingPercent))
		out.WriteString("</span><br><span class=\"muted\">")
		out.WriteString(html.EscapeString(account.QuotaMode))
		out.WriteString(" · ")
		out.WriteString(html.EscapeString(strings.Join(account.ActiveWindows, ", ")))
		out.WriteString("</span>")
		renderQuotaLines(&out, account)
		out.WriteString("</td><td class=\"nowrap\">")
		out.WriteString(strconv.Itoa(account.InflightCount))
		out.WriteString("<br><span class=\"muted\">guard reserve ")
		out.WriteString(fmt.Sprintf("%.2f%%", status.Config.MinRemainingPercent))
		out.WriteString("</span>")
		if account.InflightCount > 0 && status.Config.InflightReserveScore > 0 {
			out.WriteString("<br><span class=\"muted\">inflight score ")
			out.WriteString(formatScore(float64(account.InflightCount) * status.Config.InflightReserveScore))
			out.WriteString("</span>")
		}
		out.WriteString("</td><td class=\"tight\">")
		out.WriteString(html.EscapeString(recentSummary(account)))
		out.WriteString("</td><td>")
		if !account.LastQuotaRefreshAt.IsZero() {
			out.WriteString(html.EscapeString(account.LastQuotaRefreshAt.Format(time.RFC3339)))
		}
		if account.LastQuotaRefreshError != "" {
			out.WriteString("<br><span class=\"low\">")
			out.WriteString(html.EscapeString(account.LastQuotaRefreshError))
			out.WriteString("</span>")
		}
		out.WriteString("</td><td>")
		out.WriteString("<button data-refresh=\"")
		out.WriteString(html.EscapeString(account.AuthIndex))
		out.WriteString("\">Refresh</button>")
		if !account.HostMatched {
			out.WriteString(" <button class=\"secondary\" data-delete-auth=\"")
			out.WriteString(html.EscapeString(account.AuthID))
			out.WriteString("\" data-delete-index=\"")
			out.WriteString(html.EscapeString(account.AuthIndex))
			out.WriteString("\">Remove</button>")
		}
		out.WriteString("</td></tr>")
	}
	out.WriteString("</tbody></table><h2>Config</h2><pre>")
	out.WriteString(html.EscapeString(prettyJSON(status.Config)))
	out.WriteString("</pre>")
	out.WriteString(quotaGuardStatusScript)
	out.WriteString("</main></body></html>")
	return out.Bytes()
}

func renderAffinitySection(out *bytes.Buffer, affinity affinitySnapshot, accounts []accountSnapshot) {
	out.WriteString("<div class=\"section\"><h2>Affinity</h2>")
	out.WriteString("<p>")
	if affinity.Enabled {
		out.WriteString("<span class=\"pill good\">enabled</span>")
	} else {
		out.WriteString("<span class=\"pill\">disabled</span>")
	}
	out.WriteString(" header <code>")
	out.WriteString(html.EscapeString(firstNonEmpty(affinity.Header, "X-CPA-Client-ID")))
	out.WriteString("</code> <span class=\"muted\">missing header uses legacy/global primary</span></p>")
	out.WriteString("<p class=\"muted\">Weight is the summed estimated capacity score for a group. New clients are assigned by binding count divided by weight; auto groups select the main Plus/Team account first and use Pro/repeatable members only as backups.</p>")
	if affinity.Rebalance.LastAnalysisAt.IsZero() {
		out.WriteString("<p class=\"muted\">Rebalance has not collected a Keeper usage snapshot yet.</p>")
	} else {
		out.WriteString("<p class=\"muted\">Rebalance ")
		out.WriteString(html.EscapeString(affinity.Rebalance.LastAnalysisAt.Format(time.RFC3339)))
		if affinity.Rebalance.LastError != "" {
			out.WriteString(" · <span class=\"low\">")
			out.WriteString(html.EscapeString(affinity.Rebalance.LastError))
			out.WriteString("</span>")
		}
		out.WriteString("</p>")
	}
	out.WriteString("<div class=\"toolbar\"><button class=\"secondary\" id=\"quota-guard-rebalance-analyze\">Analyze Now</button><button class=\"secondary\" id=\"quota-guard-rebalance-once\">Rebalance Once</button></div>")
	renderManualGroupEditor(out, accounts)
	if len(affinity.Groups) > 0 {
		out.WriteString("<table><thead><tr><th>Group</th><th>Members</th><th>Current</th><th>Status</th><th>60m Load</th><th>Bindings</th><th>Action</th></tr></thead><tbody>")
		for _, group := range affinity.Groups {
			out.WriteString("<tr><td><code>")
			out.WriteString(html.EscapeString(group.ID))
			out.WriteString("</code><br><span class=\"muted\">")
			out.WriteString(html.EscapeString(firstNonEmpty(group.Source, "auto")))
			out.WriteString(" weight ")
			out.WriteString(formatScore(group.Weight))
			out.WriteString("</span></td><td>")
			for _, member := range group.Members {
				out.WriteString("<div><code>")
				out.WriteString(html.EscapeString(member))
				out.WriteString("</code>")
				switch {
				case member == group.MainAuthID:
					out.WriteString(" <span class=\"pill good\">main</span>")
				case stringSliceContains(group.BackupAuthIDs, member):
					out.WriteString(" <span class=\"pill\">backup</span>")
				}
				out.WriteString("</div>")
			}
			out.WriteString("</td><td>")
			if group.CurrentAuthID != "" {
				out.WriteString("<code>")
				out.WriteString(html.EscapeString(group.CurrentAuthID))
				out.WriteString("</code>")
				if group.CurrentAuthIndex != "" {
					out.WriteString("<br><span class=\"muted\">")
					out.WriteString(html.EscapeString(group.CurrentAuthIndex))
					out.WriteString("</span>")
				}
			} else {
				out.WriteString("<span class=\"muted\">none yet</span>")
			}
			out.WriteString("</td><td>")
			if group.Eligible {
				out.WriteString("<span class=\"pill good\">eligible</span>")
			} else {
				out.WriteString("<span class=\"pill bad\">skipped</span>")
			}
			if group.Reason != "" {
				out.WriteString("<br><span class=\"muted\">")
				out.WriteString(html.EscapeString(group.Reason))
				out.WriteString("</span>")
			}
			out.WriteString("</td><td class=\"nowrap\">")
			out.WriteString(formatScore(group.Tokens60m))
			out.WriteString(" predicted<br><span class=\"muted\">")
			out.WriteString(strconv.FormatInt(affinity.FastWindowMinutes, 10))
			out.WriteString("m ")
			out.WriteString(formatScore(group.FastTokens))
			out.WriteString(" · 60m ")
			out.WriteString(formatScore(group.SlowTokens))
			out.WriteString("<br>actual ")
			out.WriteString(fmt.Sprintf("%.2f%%", group.ActualShare))
			out.WriteString(" · target ")
			out.WriteString(fmt.Sprintf("%.2f%%", group.TargetShare))
			out.WriteString("<br>factor ")
			out.WriteString(fmt.Sprintf("%.2fx", group.LoadFactor))
			out.WriteString(" · streak ")
			out.WriteString(strconv.Itoa(group.OverloadStreak))
			out.WriteString(" · capacity ")
			out.WriteString(formatScore(group.MainCapacity))
			out.WriteString("</span>")
			out.WriteString("</td><td>")
			out.WriteString(strconv.Itoa(group.BindingCount))
			out.WriteString("</td><td>")
			if group.Source == "manual-state" {
				out.WriteString("<button class=\"secondary\" data-delete-group=\"")
				out.WriteString(html.EscapeString(group.ID))
				out.WriteString("\">Delete</button>")
			} else if group.Source == "auto" {
				out.WriteString("<button class=\"secondary\" data-create-group=\"")
				out.WriteString(html.EscapeString(group.ID))
				out.WriteString("\" data-members=\"")
				out.WriteString(html.EscapeString(strings.Join(group.Members, ",")))
				out.WriteString("\">Create Manual Override</button>")
			} else {
				out.WriteString("<span class=\"muted\">")
				out.WriteString(html.EscapeString(firstNonEmpty(group.Source, "auto")))
				out.WriteString("</span>")
			}
			out.WriteString("</td></tr>")
		}
		out.WriteString("</tbody></table>")
	}
	if len(affinity.Bindings) > 0 {
		out.WriteString("<details><summary>Client Bindings (")
		out.WriteString(strconv.Itoa(len(affinity.Bindings)))
		out.WriteString(")</summary><div class=\"toolbar\"><button class=\"secondary\" id=\"quota-guard-delete-bindings\">Delete Selected</button><select id=\"quota-guard-move-target\"><option value=\"\">Move to group...</option>")
		for _, group := range affinity.Groups {
			if !group.Eligible {
				continue
			}
			out.WriteString("<option value=\"")
			out.WriteString(html.EscapeString(group.ID))
			out.WriteString("\">")
			out.WriteString(html.EscapeString(group.ID))
			out.WriteString(" (")
			out.WriteString(strconv.Itoa(group.BindingCount))
			out.WriteString(")</option>")
		}
		out.WriteString("</select><button class=\"secondary\" id=\"quota-guard-move-bindings\">Move Selected</button></div><table><thead><tr><th><input type=\"checkbox\" id=\"quota-guard-bindings-all\"></th><th>Client</th><th>Group</th><th>60m Activity</th><th>Last Seen</th><th>Move State</th></tr></thead><tbody>")
		for _, binding := range affinity.Bindings {
			out.WriteString("<tr><td><input type=\"checkbox\" name=\"quota-guard-client-binding\" value=\"")
			out.WriteString(html.EscapeString(binding.ClientID))
			out.WriteString("\"></td><td><code>")
			out.WriteString(html.EscapeString(binding.ClientID))
			out.WriteString("</code></td><td><code>")
			out.WriteString(html.EscapeString(binding.GroupID))
			out.WriteString("</code></td><td class=\"nowrap\">")
			out.WriteString(strconv.Itoa(binding.Picks60m))
			out.WriteString(" picks<br><span class=\"muted\">")
			out.WriteString(formatScore(binding.UsageScore60m))
			out.WriteString(" local score</span></td><td>")
			if !binding.LastSeenAt.IsZero() {
				out.WriteString(html.EscapeString(binding.LastSeenAt.Format(time.RFC3339)))
			}
			out.WriteString("</td><td class=\"tight\">")
			if !binding.CooldownUntil.IsZero() && binding.CooldownUntil.After(time.Now()) {
				out.WriteString("<span class=\"pill\">cooldown</span><br><span class=\"muted\">")
				out.WriteString(html.EscapeString(binding.CooldownUntil.Format("01-02 15:04")))
				out.WriteString("</span>")
			} else {
				out.WriteString("<span class=\"pill good\">movable when idle</span>")
			}
			if binding.LastMoveReason != "" {
				out.WriteString("<br><span class=\"muted\">")
				out.WriteString(html.EscapeString(binding.LastMoveReason))
				out.WriteString("</span>")
			}
			out.WriteString("</td></tr>")
		}
		out.WriteString("</tbody></table></details>")
	}
	if len(affinity.Rebalance.History) > 0 {
		out.WriteString("<details><summary>Rebalance History (" + strconv.Itoa(len(affinity.Rebalance.History)) + ")</summary><table><thead><tr><th>Time</th><th>Result</th><th>Client</th><th>Route</th><th>Reason</th></tr></thead><tbody>")
		for i := len(affinity.Rebalance.History) - 1; i >= 0 && i >= len(affinity.Rebalance.History)-50; i-- {
			entry := affinity.Rebalance.History[i]
			out.WriteString("<tr><td class=\"nowrap\">" + html.EscapeString(entry.At.Format("01-02 15:04")) + "</td><td>" + html.EscapeString(entry.Action+" / "+entry.Result) + "</td><td><code>" + html.EscapeString(entry.ClientID) + "</code></td><td><code>" + html.EscapeString(entry.FromGroup) + "</code> → <code>" + html.EscapeString(entry.ToGroup) + "</code></td><td>" + html.EscapeString(entry.Reason) + "</td></tr>")
		}
		out.WriteString("</tbody></table></details>")
	}
	out.WriteString("</div>")
}

func renderManualGroupEditor(out *bytes.Buffer, accounts []accountSnapshot) {
	editable := manualCalibrateAccounts(accounts)
	if len(editable) == 0 {
		return
	}
	out.WriteString("<details id=\"quota-guard-manual-group-details\"><summary>Manual Groups</summary><form class=\"manual\" id=\"quota-guard-manual-group\"><input name=\"group_id\" placeholder=\"group id\" required>")
	out.WriteString("<div>")
	for _, account := range editable {
		out.WriteString("<label class=\"pill\"><input type=\"checkbox\" name=\"member\" value=\"")
		out.WriteString(html.EscapeString(account.AuthID))
		out.WriteString("\"> ")
		out.WriteString(html.EscapeString(manualCalibrateAccountLabel(account)))
		out.WriteString("</label> ")
	}
	out.WriteString("</div><button>Save Group</button></form></details>")
}

func renderQuotaLines(out *bytes.Buffer, account accountSnapshot) {
	out.WriteString("<div class=\"quota-lines\">")
	for _, window := range account.ActiveWindows {
		remaining := account.WindowRemaining[window]
		out.WriteString("<div class=\"quota-line\"><span class=\"quota-label\">")
		out.WriteString(html.EscapeString(window))
		out.WriteString("</span><span>")
		out.WriteString(fmt.Sprintf("%.2f%% left", remaining))
		out.WriteString("</span></div>")
		if since := account.UsedSinceSnapshot[window]; since > 0 {
			out.WriteString("<div class=\"muted\">since refresh ")
			out.WriteString(formatScore(since))
			out.WriteString("</div>")
		}
		if snap, ok := account.QuotaSnapshots[window]; ok {
			if snap.PlanType != "" || snap.LimitScore > 0 {
				out.WriteString("<div class=\"muted\">")
				if snap.PlanType != "" {
					out.WriteString("plan ")
					out.WriteString(html.EscapeString(snap.PlanType))
				}
				if snap.LimitScore > 0 {
					if snap.PlanType != "" {
						out.WriteString(" · ")
					}
					out.WriteString("local base ")
					out.WriteString(formatScore(snap.LimitScore))
				}
				out.WriteString("</div>")
			}
			if snap.ResetAt != nil {
				out.WriteString("<div class=\"muted\">reset ")
				out.WriteString(html.EscapeString(snap.ResetAt.Format("01-02 15:04")))
				out.WriteString("</div>")
			}
		}
	}
	out.WriteString("</div>")
}

func manualCalibrateAccountLabel(account accountSnapshot) string {
	parts := []string{account.AuthID}
	if account.AuthIndex != "" {
		parts = append(parts, account.AuthIndex)
	}
	if account.Provider != "" {
		parts = append(parts, account.Provider)
	}
	return strings.Join(parts, " · ")
}

func manualCalibrateAccounts(accounts []accountSnapshot) []accountSnapshot {
	byIndex := map[string]bool{}
	for _, account := range accounts {
		if account.AuthIndex != "" && account.AuthID != account.AuthIndex {
			byIndex[account.AuthIndex] = true
		}
	}
	out := make([]accountSnapshot, 0, len(accounts))
	for _, account := range accounts {
		if account.AuthID == "" {
			continue
		}
		if account.AuthID == account.AuthIndex && byIndex[account.AuthIndex] {
			continue
		}
		if account.AuthIndex == "" && byIndex[account.AuthID] {
			continue
		}
		out = append(out, account)
	}
	return out
}

func recentSummary(account accountSnapshot) string {
	summary := fmt.Sprintf("ok %d / fail %d", account.Success, account.Failed)
	for i := len(account.RecentRequests) - 1; i >= 0; i-- {
		bucket := account.RecentRequests[i]
		if bucket.Success == 0 && bucket.Failed == 0 {
			continue
		}
		return fmt.Sprintf("%s\nlast %s: ok %d / fail %d", summary, bucket.Time, bucket.Success, bucket.Failed)
	}
	return summary
}

func formatScore(score float64) string {
	switch {
	case score >= 1000000:
		return fmt.Sprintf("%.2fm", score/1000000)
	case score >= 1000:
		return fmt.Sprintf("%.1fk", score/1000)
	default:
		return fmt.Sprintf("%.0f", score)
	}
}

const quotaGuardStatusScript = `<script>
(function(){
  const groupForm = document.getElementById("quota-guard-manual-group");
  const groupDetails = document.getElementById("quota-guard-manual-group-details");
  const message = document.getElementById("quota-guard-message");
  const refreshAll = document.getElementById("quota-guard-refresh-all");
  const bindingSelectAll = document.getElementById("quota-guard-bindings-all");
  const deleteBindings = document.getElementById("quota-guard-delete-bindings");
  const moveBindings = document.getElementById("quota-guard-move-bindings");
  const moveTarget = document.getElementById("quota-guard-move-target");
  const analyzeRebalance = document.getElementById("quota-guard-rebalance-analyze");
  const rebalanceOnce = document.getElementById("quota-guard-rebalance-once");
  async function action(data) {
    const params = new URLSearchParams();
    Object.keys(data || {}).forEach(function(key) {
      const value = data[key];
      if (Array.isArray(value)) {
        value.forEach(function(item) {
          if (item !== undefined && item !== "") params.append(key, String(item));
        });
      } else if (value !== undefined && value !== "") {
        params.set(key, String(value));
      }
    });
    const response = await fetch("/v0/resource/plugins/quota-guard/status?" + params.toString(), {method: "GET"});
    if (!response.ok) {
      let text = await response.text();
      try { text = JSON.parse(text).error || text; } catch (_) {}
      throw new Error(text || ("HTTP " + response.status));
    }
    return response;
  }
  async function refresh(data) {
    message.textContent = "Refreshing...";
    try {
      await action(Object.assign({action:"refresh"}, data || {}));
      message.textContent = "Refreshed.";
      setTimeout(function(){ location.reload(); }, 500);
    } catch (err) {
      message.textContent = err.message;
    }
  }
  if (refreshAll) refreshAll.addEventListener("click", function(){ refresh({all:true, force:true}); });
  if (analyzeRebalance) analyzeRebalance.addEventListener("click", async function() {
    message.textContent = "Analyzing Keeper usage...";
    try {
      await action({action:"rebalance-analyze"});
      message.textContent = "Rebalance analysis completed.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  if (rebalanceOnce) rebalanceOnce.addEventListener("click", async function() {
    if (!confirm("Run one guarded rebalance move if an eligible idle client is found?")) return;
    message.textContent = "Running guarded rebalance...";
    try {
      await action({action:"rebalance-once"});
      message.textContent = "Rebalance cycle completed.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  document.addEventListener("click", function(event) {
    const button = event.target.closest("button[data-refresh]");
    if (!button) return;
    refresh({auth_index: button.getAttribute("data-refresh"), force:true});
  });
  document.addEventListener("click", async function(event) {
    const button = event.target.closest("button[data-delete-auth]");
    if (!button) return;
    if (!confirm("Remove this local quota-guard state entry?")) return;
    message.textContent = "Removing...";
    try {
      await action({action:"delete-state", auth_id:button.getAttribute("data-delete-auth"), auth_index:button.getAttribute("data-delete-index")});
      message.textContent = "Removed.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  document.addEventListener("click", async function(event) {
    const button = event.target.closest("button[data-delete-group]");
    if (!button) return;
    if (!confirm("Delete this manual group?")) return;
    message.textContent = "Deleting group...";
    try {
      await action({action:"delete-manual-group", group_id:button.getAttribute("data-delete-group")});
      message.textContent = "Deleted.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  document.addEventListener("click", function(event) {
    const button = event.target.closest("button[data-create-group]");
    if (!button || !groupForm) return;
    const sourceGroup = button.getAttribute("data-create-group") || "group";
    const input = groupForm.querySelector("input[name=group_id]");
    if (input) input.value = "manual-" + sourceGroup.replace(/^auto-/, "");
    const members = (button.getAttribute("data-members") || "").split(",").filter(Boolean);
    groupForm.querySelectorAll("input[name=member]").forEach(function(box) {
      box.checked = members.indexOf(box.value) !== -1;
    });
    if (groupDetails) groupDetails.open = true;
    if (input) input.focus();
  });
  if (bindingSelectAll) bindingSelectAll.addEventListener("change", function() {
    document.querySelectorAll("input[name=quota-guard-client-binding]").forEach(function(box) {
      box.checked = bindingSelectAll.checked;
    });
  });
  if (deleteBindings) deleteBindings.addEventListener("click", async function() {
    const selected = Array.from(document.querySelectorAll("input[name=quota-guard-client-binding]:checked")).map(function(box) { return box.value; });
    if (selected.length === 0) {
      message.textContent = "Select client bindings to delete.";
      return;
    }
    if (!confirm("Delete selected client bindings?")) return;
    message.textContent = "Deleting client bindings...";
    try {
      await action({action:"delete-client-bindings", client_id:selected});
      message.textContent = "Client bindings deleted.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  if (moveBindings) moveBindings.addEventListener("click", async function() {
    const selected = Array.from(document.querySelectorAll("input[name=quota-guard-client-binding]:checked")).map(function(box) { return box.value; });
    const groupID = moveTarget ? moveTarget.value : "";
    if (selected.length === 0) {
      message.textContent = "Select client bindings to move.";
      return;
    }
    if (!groupID) {
      message.textContent = "Select a target group.";
      return;
    }
    if (!confirm("Move selected client bindings to " + groupID + "?")) return;
    message.textContent = "Moving client bindings...";
    try {
      await action({action:"move-client-bindings", client_id:selected, group_id:groupID});
      message.textContent = "Client bindings moved.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  if (groupForm) groupForm.addEventListener("submit", async function(event) {
    event.preventDefault();
    message.textContent = "Saving group...";
    const formData = new FormData(groupForm);
    const data = {group_id: formData.get("group_id"), member: formData.getAll("member")};
    try {
      await action(Object.assign({action:"save-manual-group"}, data));
      message.textContent = "Group saved.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
})();
</script>`

func prettyJSON(v any) string {
	raw, errMarshal := json.MarshalIndent(v, "", "  ")
	if errMarshal != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

func callHostAuthList() (authListResponse, error) {
	result, errCall := callHostFunc(pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return authListResponse{}, errCall
	}
	var resp authListResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return authListResponse{}, fmt.Errorf("decode host.auth.list result: %w", errUnmarshal)
	}
	return resp, nil
}

func callHostAuthGet(authIndex string) (authGetResponse, error) {
	result, errCall := callHostFunc(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if errCall != nil {
		return authGetResponse{}, errCall
	}
	var resp authGetResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return authGetResponse{}, fmt.Errorf("decode host.auth.get result: %w", errUnmarshal)
	}
	return resp, nil
}

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
