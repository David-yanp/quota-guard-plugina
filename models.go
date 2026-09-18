package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
	Enabled                               bool                           `yaml:"enabled" json:"enabled"`
	Priority                              int                            `yaml:"priority" json:"priority"`
	StateFile                             string                         `yaml:"state_file" json:"state_file"`
	MinRemainingPercent                   float64                        `yaml:"min_remaining_percent" json:"min_remaining_percent"`
	StickyCurrentAuthSeconds              int64                          `yaml:"sticky_current_auth_seconds" json:"sticky_current_auth_seconds"`
	FailWhenAllLow                        bool                           `yaml:"fail_when_all_low" json:"fail_when_all_low"`
	DelegateWhenUnconfigured              string                         `yaml:"delegate_when_unconfigured" json:"delegate_when_unconfigured"`
	Default5hLimitScore                   float64                        `yaml:"default_5h_limit_score" json:"default_5h_limit_score"`
	Default7dLimitScore                   float64                        `yaml:"default_7d_limit_score" json:"default_7d_limit_score"`
	DefaultMonthlyLimitScore              float64                        `yaml:"default_monthly_limit_score" json:"default_monthly_limit_score"`
	ProLimitMultiplier                    float64                        `yaml:"pro_limit_multiplier" json:"pro_limit_multiplier"`
	InflightReserveScore                  float64                        `yaml:"inflight_reserve_score" json:"inflight_reserve_score"`
	MaxInflightAgeSeconds                 int64                          `yaml:"max_inflight_age_seconds" json:"max_inflight_age_seconds"`
	CountFailedRequests                   bool                           `yaml:"count_failed_requests" json:"count_failed_requests"`
	InputWeight                           float64                        `yaml:"input_weight" json:"input_weight"`
	OutputWeight                          float64                        `yaml:"output_weight" json:"output_weight"`
	ReasoningWeight                       float64                        `yaml:"reasoning_weight" json:"reasoning_weight"`
	CachedWeight                          float64                        `yaml:"cached_weight" json:"cached_weight"`
	RequestScore                          float64                        `yaml:"request_score" json:"request_score"`
	QuotaQueryURL                         string                         `yaml:"quota_query_url" json:"quota_query_url,omitempty"`
	QuotaQueryMinIntervalSecs             int64                          `yaml:"quota_query_min_interval_seconds" json:"quota_query_min_interval_seconds,omitempty"`
	QuotaRefreshEnabled                   bool                           `yaml:"quota_refresh_enabled" json:"quota_refresh_enabled"`
	QuotaRefreshTriggerEndpoint           string                         `yaml:"quota_refresh_trigger_endpoint" json:"quota_refresh_trigger_endpoint,omitempty"`
	QuotaRefreshTriggerWaitSecs           int64                          `yaml:"quota_refresh_trigger_wait_seconds" json:"quota_refresh_trigger_wait_seconds"`
	QuotaRefreshEndpoint                  string                         `yaml:"quota_refresh_endpoint" json:"quota_refresh_endpoint,omitempty"`
	QuotaRefreshIntervalSecs              int64                          `yaml:"quota_refresh_interval_seconds" json:"quota_refresh_interval_seconds"`
	QuotaRefreshMinIntervalSecs           int64                          `yaml:"quota_refresh_min_interval_per_auth_seconds" json:"quota_refresh_min_interval_per_auth_seconds"`
	QuotaRefreshTimeoutSecs               int64                          `yaml:"quota_refresh_timeout_seconds" json:"quota_refresh_timeout_seconds"`
	QuotaRefreshOnStartup                 bool                           `yaml:"quota_refresh_on_startup" json:"quota_refresh_on_startup"`
	QuotaSnapshotMaxAgeSecs               int64                          `yaml:"quota_snapshot_max_age_seconds" json:"quota_snapshot_max_age_seconds"`
	ResourceActionsRequireManagementKey   bool                           `yaml:"resource_actions_require_management_key" json:"resource_actions_require_management_key"`
	RequestErrorStatusOverrideEnabled     bool                           `yaml:"request_error_status_override_enabled" json:"request_error_status_override_enabled"`
	ClientAffinityEnabled                 bool                           `yaml:"client_affinity_enabled" json:"client_affinity_enabled"`
	LegacyPrimaryAuth                     string                         `yaml:"legacy_primary_auth" json:"legacy_primary_auth,omitempty"`
	ClientAffinityHeader                  string                         `yaml:"client_affinity_header" json:"client_affinity_header"`
	ClientAffinityGroupMinSize            int                            `yaml:"client_affinity_group_min_size" json:"client_affinity_group_min_size"`
	ClientAffinityAssignmentMode          string                         `yaml:"client_affinity_assignment_mode" json:"client_affinity_assignment_mode"`
	ClientAffinityStorePlainID            bool                           `yaml:"client_affinity_store_plain_id" json:"client_affinity_store_plain_id"`
	ClientAffinityAutoWeightByQuota       bool                           `yaml:"client_affinity_auto_weight_by_quota" json:"client_affinity_auto_weight_by_quota"`
	ClientAffinityGroups                  map[string][]string            `yaml:"client_affinity_groups,omitempty" json:"client_affinity_groups,omitempty"`
	ClientAffinityRepeatableAuths         []string                       `yaml:"client_affinity_repeatable_auths,omitempty" json:"client_affinity_repeatable_auths,omitempty"`
	ClientAffinityAPIKeyMap               map[string]string              `yaml:"client_affinity_api_key_map,omitempty" json:"client_affinity_api_key_map,omitempty"`
	ClientAffinityExclusiveAuths          map[string]exclusiveAuthConfig `yaml:"client_affinity_exclusive_auths,omitempty" json:"client_affinity_exclusive_auths,omitempty"`
	ClientAffinityMinLoadEnabled          bool                           `yaml:"client_affinity_min_load_enabled" json:"client_affinity_min_load_enabled"`
	ClientAffinityMinBindingPerGroup      int                            `yaml:"client_affinity_min_binding_per_group" json:"client_affinity_min_binding_per_group"`
	ClientAffinityMinTrafficPercent       float64                        `yaml:"client_affinity_min_traffic_percent" json:"client_affinity_min_traffic_percent"`
	ClientAffinityDormantSecs             int64                          `yaml:"client_affinity_dormant_seconds" json:"client_affinity_dormant_seconds"`
	ClientAffinityRebalanceAPIKeyEnabled  bool                           `yaml:"client_affinity_rebalance_api_key_enabled" json:"client_affinity_rebalance_api_key_enabled"`
	ClientAffinityRebalanceAPIKeyUsageURL string                         `yaml:"client_affinity_rebalance_api_key_usage_endpoint" json:"client_affinity_rebalance_api_key_usage_endpoint,omitempty"`
	ClientAffinityRebalanceAPIKeyRefresh  int64                          `yaml:"client_affinity_rebalance_api_key_refresh_seconds" json:"client_affinity_rebalance_api_key_refresh_seconds"`
	ClientAffinityUnknownClientPolicy     string                         `yaml:"client_affinity_rebalance_unknown_client_policy" json:"client_affinity_rebalance_unknown_client_policy"`
	ClientAffinityUnattributedPolicy      string                         `yaml:"client_affinity_rebalance_unattributed_policy" json:"client_affinity_rebalance_unattributed_policy"`
	ClientAffinityRebalanceEnabled        bool                           `yaml:"client_affinity_rebalance_enabled" json:"client_affinity_rebalance_enabled"`
	ClientAffinityRebalanceMode           string                         `yaml:"client_affinity_rebalance_mode" json:"client_affinity_rebalance_mode"`
	ClientAffinityRebalanceUsageURL       string                         `yaml:"client_affinity_rebalance_usage_endpoint" json:"client_affinity_rebalance_usage_endpoint,omitempty"`
	ClientAffinityRebalanceIntervalSecs   int64                          `yaml:"client_affinity_rebalance_interval_seconds" json:"client_affinity_rebalance_interval_seconds"`
	ClientAffinityRebalanceWindowMins     int64                          `yaml:"client_affinity_rebalance_window_minutes" json:"client_affinity_rebalance_window_minutes"`
	ClientAffinityRebalanceIdleSecs       int64                          `yaml:"client_affinity_rebalance_idle_seconds" json:"client_affinity_rebalance_idle_seconds"`
	ClientAffinityRebalanceCooldownSecs   int64                          `yaml:"client_affinity_rebalance_cooldown_seconds" json:"client_affinity_rebalance_cooldown_seconds"`
	ClientAffinityManualCooldownSecs      int64                          `yaml:"client_affinity_manual_move_cooldown_seconds" json:"client_affinity_manual_move_cooldown_seconds"`
	ClientAffinityRebalanceWarmupSecs     int64                          `yaml:"client_affinity_rebalance_warmup_seconds" json:"client_affinity_rebalance_warmup_seconds"`
	ClientAffinityRebalanceMaxMoves       int                            `yaml:"client_affinity_rebalance_max_moves_per_cycle" json:"client_affinity_rebalance_max_moves_per_cycle"`
	ClientAffinityRebalanceMinLoadRatio   float64                        `yaml:"client_affinity_rebalance_min_load_ratio" json:"client_affinity_rebalance_min_load_ratio"`
	ClientAffinityRebalanceMinImprove     float64                        `yaml:"client_affinity_rebalance_min_improvement_percent" json:"client_affinity_rebalance_min_improvement_percent"`
	ClientAffinityRebalanceHistoryLimit   int                            `yaml:"client_affinity_rebalance_history_limit" json:"client_affinity_rebalance_history_limit"`
	ClientAffinityRebalanceFastWindowMins int64                          `yaml:"client_affinity_rebalance_fast_window_minutes" json:"client_affinity_rebalance_fast_window_minutes"`
	ClientAffinityRebalanceFastWeight     float64                        `yaml:"client_affinity_rebalance_fast_weight" json:"client_affinity_rebalance_fast_weight"`
	ClientAffinityRebalanceOverload       float64                        `yaml:"client_affinity_rebalance_overload_threshold" json:"client_affinity_rebalance_overload_threshold"`
	ClientAffinityRebalanceTarget         float64                        `yaml:"client_affinity_rebalance_target_threshold" json:"client_affinity_rebalance_target_threshold"`
	ClientAffinityRebalanceStreak         int                            `yaml:"client_affinity_rebalance_overload_consecutive" json:"client_affinity_rebalance_overload_consecutive"`
	WeeklyBudgetEnabled                   bool                           `yaml:"weekly_budget_enabled" json:"weekly_budget_enabled"`
	WeeklyBudgetTimezoneOffsetHours       int                            `yaml:"weekly_budget_timezone_offset_hours" json:"weekly_budget_timezone_offset_hours"`
	WeeklyBudgetMaxAdjustmentPercent      float64                        `yaml:"weekly_budget_max_adjustment_percent" json:"weekly_budget_max_adjustment_percent"`
	ProxyRestoreEnabled                   bool                           `yaml:"proxy_restore_enabled" json:"proxy_restore_enabled"`
	ProxyMapFile                          string                         `yaml:"proxy_map_file" json:"proxy_map_file"`
	ProxyRestoreIntervalSecs              int64                          `yaml:"proxy_restore_interval_seconds" json:"proxy_restore_interval_seconds"`
	ProxyRestoreSettleSecs                int64                          `yaml:"proxy_restore_settle_seconds" json:"proxy_restore_settle_seconds"`
	ProxyRestoreBlockMissing              bool                           `yaml:"proxy_restore_block_missing" json:"proxy_restore_block_missing"`
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
		ClientAffinityAPIKeyMap:               map[string]string{},
		ClientAffinityExclusiveAuths:          map[string]exclusiveAuthConfig{},
		ClientAffinityMinLoadEnabled:          true,
		ClientAffinityMinBindingPerGroup:      1,
		ClientAffinityMinTrafficPercent:       5,
		ClientAffinityDormantSecs:             86400,
		ClientAffinityRebalanceAPIKeyEnabled:  true,
		ClientAffinityRebalanceAPIKeyUsageURL: "http://cpa-usage-keeper:8080/cpa/api/v1/usage/overview/realtime?window=60m&api_key_id={api_key_id}",
		ClientAffinityRebalanceAPIKeyRefresh:  300,
		ClientAffinityUnknownClientPolicy:     "local-fallback",
		ClientAffinityUnattributedPolicy:      "observe-only",
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
		WeeklyBudgetEnabled:                   true,
		WeeklyBudgetTimezoneOffsetHours:       8,
		WeeklyBudgetMaxAdjustmentPercent:      20,
		ProxyRestoreEnabled:                   false,
		ProxyMapFile:                          "plugins/quota-guard-proxy-map.json",
		ProxyRestoreIntervalSecs:              15,
		ProxyRestoreSettleSecs:                3,
		ProxyRestoreBlockMissing:              true,
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
	Version          int                             `json:"version"`
	SavedAt          time.Time                       `json:"saved_at"`
	CurrentAuthID    string                          `json:"current_auth_id,omitempty"`
	CurrentAuthIndex string                          `json:"current_auth_index,omitempty"`
	CurrentRole      string                          `json:"current_role,omitempty"`
	LastSelectedAt   time.Time                       `json:"last_selected_at,omitempty"`
	Accounts         map[string]*accountState        `json:"accounts"`
	ClientBindings   map[string]*clientBindingState  `json:"client_bindings,omitempty"`
	ManualGroups     map[string][]string             `json:"manual_groups,omitempty"`
	Groups           map[string]*affinityGroupState  `json:"groups,omitempty"`
	GroupCurrent     map[string]*groupCurrentState   `json:"group_current,omitempty"`
	ClientActivity   []clientActivityEvent           `json:"client_activity,omitempty"`
	Rebalance        rebalanceState                  `json:"rebalance,omitempty"`
	ExclusiveOwners  map[string]*exclusiveOwnerState `json:"exclusive_owners,omitempty"`
}

type exclusiveAuthConfig struct {
	MaxClients            int   `yaml:"max_clients" json:"max_clients"`
	IdleReleaseSeconds    int64 `yaml:"idle_release_seconds" json:"idle_release_seconds"`
	PreserveOwnerFailover bool  `yaml:"preserve_owner_during_failover" json:"preserve_owner_during_failover"`
	AllowClientlessClaim  bool  `yaml:"allow_clientless_claim" json:"allow_clientless_claim"`
}

type exclusiveOwnerState struct {
	AuthID         string    `json:"auth_id"`
	AuthIndex      string    `json:"auth_index,omitempty"`
	ClientID       string    `json:"client_id"`
	LastActivityAt time.Time `json:"last_activity_at"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

type accountState struct {
	AuthID                    string                             `json:"auth_id"`
	AuthIndex                 string                             `json:"auth_index,omitempty"`
	Provider                  string                             `json:"provider,omitempty"`
	Priority                  int                                `json:"priority,omitempty"`
	Status                    string                             `json:"status,omitempty"`
	StatusMessage             string                             `json:"status_message,omitempty"`
	Disabled                  bool                               `json:"disabled,omitempty"`
	Unavailable               bool                               `json:"unavailable,omitempty"`
	NextRetryAfter            time.Time                          `json:"next_retry_after,omitempty"`
	UnknownStatusSeen         int64                              `json:"unknown_status_seen,omitempty"`
	LastUnknownStatusAt       time.Time                          `json:"last_unknown_status_at,omitempty"`
	LastUnknownStatusLogAt    time.Time                          `json:"last_unknown_status_log_at,omitempty"`
	Success                   int64                              `json:"success,omitempty"`
	Failed                    int64                              `json:"failed,omitempty"`
	RecentRequests            []pluginapi.HostRecentRequestEntry `json:"recent_requests,omitempty"`
	Limits                    map[string]float64                 `json:"limits"`
	ActiveWindows             map[string]bool                    `json:"active_windows"`
	QuotaSnapshots            map[string]quotaWindowSnapshot     `json:"quota_snapshots,omitempty"`
	Events                    []usageEvent                       `json:"events,omitempty"`
	Inflight                  []inflightReserve                  `json:"inflight,omitempty"`
	Calibration               map[string]calib                   `json:"calibration,omitempty"`
	LastUsageAt               time.Time                          `json:"last_usage_at,omitempty"`
	LastQueryAt               map[string]time.Time               `json:"last_query_at,omitempty"`
	LastQuotaRefreshAttemptAt time.Time                          `json:"last_quota_refresh_attempt_at,omitempty"`
	LastQuotaRefreshAt        time.Time                          `json:"last_quota_refresh_at,omitempty"`
	LastQuotaRefreshError     string                             `json:"last_quota_refresh_error,omitempty"`
	LastResetAt               map[string]time.Time               `json:"last_reset_at,omitempty"`
	WeeklyBudget              weeklyBudgetState                  `json:"weekly_budget,omitempty"`
	ProxyStatus               string                             `json:"proxy_status,omitempty"`
	ProxyDisplay              string                             `json:"proxy_display,omitempty"`
	ProxyLastCheckedAt        time.Time                          `json:"proxy_last_checked_at,omitempty"`
	ProxyLastRestoredAt       time.Time                          `json:"proxy_last_restored_at,omitempty"`
	ProxyLastError            string                             `json:"proxy_last_error,omitempty"`
	HealthObservations        map[string]*modelHealthObservation `json:"health_observations,omitempty"`
}

type modelHealthObservation struct {
	Model           string                    `json:"model"`
	APIKeyID        string                    `json:"api_key_id,omitempty"`
	CurrentInflight int                       `json:"current_inflight,omitempty"`
	MaxInflight     int                       `json:"max_inflight,omitempty"`
	LastRequestAt   time.Time                 `json:"last_request_at,omitempty"`
	LastFailureAt   time.Time                 `json:"last_failure_at,omitempty"`
	LastOverloadAt  time.Time                 `json:"last_overload_at,omitempty"`
	Buckets         []healthObservationBucket `json:"buckets,omitempty"`
}

type healthObservationBucket struct {
	At                time.Time `json:"at"`
	Requests          int64     `json:"requests,omitempty"`
	Successes         int64     `json:"successes,omitempty"`
	Failures          int64     `json:"failures,omitempty"`
	OverloadFailures  int64     `json:"overload_failures,omitempty"`
	RateLimitFailures int64     `json:"rate_limit_failures,omitempty"`
	AuthFailures      int64     `json:"auth_failures,omitempty"`
	ClientFailures    int64     `json:"client_failures,omitempty"`
	ContextFailures   int64     `json:"context_failures,omitempty"`
	OtherFailures     int64     `json:"other_failures,omitempty"`
	Tokens            float64   `json:"tokens,omitempty"`
	LatencyMillis     int64     `json:"latency_millis,omitempty"`
	LatencySamples    int64     `json:"latency_samples,omitempty"`
	TTFTMillis        int64     `json:"ttft_millis,omitempty"`
	TTFTSamples       int64     `json:"ttft_samples,omitempty"`
	MaxInflight       int       `json:"max_inflight,omitempty"`
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

type weeklyQuotaSample struct {
	LocalDate        string    `json:"local_date"`
	At               time.Time `json:"at"`
	ObservedAt       time.Time `json:"observed_at,omitempty"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
}

type weeklyBudgetState struct {
	CycleResetAt         time.Time           `json:"cycle_reset_at,omitempty"`
	LastObservedResetAt  time.Time           `json:"last_observed_reset_at,omitempty"`
	ResetDriftSeconds    int64               `json:"reset_drift_seconds,omitempty"`
	Samples              []weeklyQuotaSample `json:"samples,omitempty"`
	LastSampleAt         time.Time           `json:"last_sample_at,omitempty"`
	LastDailyBurnPercent float64             `json:"last_daily_burn_percent,omitempty"`
	ExpectedDailyBurn    float64             `json:"expected_daily_burn_percent,omitempty"`
	ExpectedRemaining    float64             `json:"expected_remaining_percent,omitempty"`
	DeviationPercent     float64             `json:"deviation_percent,omitempty"`
	AdjustmentMultiplier float64             `json:"adjustment_multiplier,omitempty"`
	Reason               string              `json:"reason,omitempty"`
}

type usageEvent struct {
	At     time.Time `json:"at"`
	Score  float64   `json:"score"`
	Model  string    `json:"model,omitempty"`
	Failed bool      `json:"failed,omitempty"`
}

type inflightReserve struct {
	At       time.Time `json:"at"`
	Model    string    `json:"model,omitempty"`
	ClientID string    `json:"client_id,omitempty"`
	APIKeyID string    `json:"api_key_id,omitempty"`
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
	ClientID            string    `json:"client_id"`
	APIKeyID            string    `json:"api_key_id,omitempty"`
	GroupID             string    `json:"group_id"`
	Source              string    `json:"source,omitempty"`
	CreatedAt           time.Time `json:"created_at,omitempty"`
	UpdatedAt           time.Time `json:"updated_at,omitempty"`
	LastSeenAt          time.Time `json:"last_seen_at,omitempty"`
	LastFailoverAt      time.Time `json:"last_failover_at,omitempty"`
	LastFailoverGroupID string    `json:"last_failover_group_id,omitempty"`
	FailoverCount       int       `json:"failover_count,omitempty"`
	LastAutoMoveAt      time.Time `json:"last_auto_move_at,omitempty"`
	LastManualMoveAt    time.Time `json:"last_manual_move_at,omitempty"`
	LastMoveReason      string    `json:"last_move_reason,omitempty"`
}

type clientActivityEvent struct {
	At       time.Time `json:"at"`
	ClientID string    `json:"client_id"`
	APIKeyID string    `json:"api_key_id,omitempty"`
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
	APIKeys     map[string]keeperUsageItem `json:"api_keys,omitempty"`
}

type keeperUsageItem struct {
	AuthIndex string  `json:"auth_index"`
	Label     string  `json:"label,omitempty"`
	Tokens    float64 `json:"tokens"`
	Requests  int64   `json:"requests"`
	Share     float64 `json:"share,omitempty"`
}

type rebalanceState struct {
	StartedAt         time.Time                  `json:"started_at,omitempty"`
	LastAttemptAt     time.Time                  `json:"last_attempt_at,omitempty"`
	LastAnalysisAt    time.Time                  `json:"last_analysis_at,omitempty"`
	LastError         string                     `json:"last_error,omitempty"`
	KeeperUsage       keeperUsageSnapshot        `json:"keeper_usage,omitempty"`
	KeeperFastUsage   keeperUsageSnapshot        `json:"keeper_fast_usage,omitempty"`
	APIKeyUsage       map[string]keeperUsageItem `json:"api_key_usage,omitempty"`
	LastAPIKeyUsageAt time.Time                  `json:"last_api_key_usage_at,omitempty"`
	Groups            map[string]groupLoadState  `json:"groups,omitempty"`
	OverloadStreak    map[string]int             `json:"overload_streak,omitempty"`
	History           []rebalanceHistoryEntry    `json:"history,omitempty"`
}

type groupLoadState struct {
	GroupID            string  `json:"group_id"`
	Tokens             float64 `json:"tokens"`
	Requests           float64 `json:"requests"`
	Capacity           float64 `json:"capacity"`
	ActualShare        float64 `json:"actual_share"`
	TargetShare        float64 `json:"target_share"`
	LoadFactor         float64 `json:"load_factor"`
	Eligible           bool    `json:"eligible"`
	Reason             string  `json:"reason,omitempty"`
	FastTokens         float64 `json:"fast_tokens,omitempty"`
	SlowTokens         float64 `json:"slow_tokens,omitempty"`
	EffectiveCapacity  float64 `json:"effective_capacity,omitempty"`
	BindingCount       int     `json:"binding_count,omitempty"`
	ActiveBindingCount int     `json:"active_binding_count,omitempty"`
	FloorState         string  `json:"floor_state,omitempty"`
	Attribution        string  `json:"attribution,omitempty"`
	APIKeyTokens       float64 `json:"api_key_tokens,omitempty"`
	LocalTokens        float64 `json:"local_tokens,omitempty"`
	UnattributedTokens float64 `json:"unattributed_tokens,omitempty"`
	FiveHourActive     bool    `json:"five_hour_active,omitempty"`
	FiveHourRemaining  float64 `json:"five_hour_remaining_percent,omitempty"`
	WeeklyRemaining    float64 `json:"weekly_remaining_percent,omitempty"`
	ExpectedRemaining  float64 `json:"expected_remaining_percent,omitempty"`
	DailyBurn          float64 `json:"daily_burn_percent,omitempty"`
	ExpectedDailyBurn  float64 `json:"expected_daily_burn_percent,omitempty"`
	BudgetMultiplier   float64 `json:"budget_multiplier,omitempty"`
	BudgetReason       string  `json:"budget_reason,omitempty"`
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
	AuthID                    string                             `json:"auth_id"`
	AuthIndex                 string                             `json:"auth_index,omitempty"`
	HostMatched               bool                               `json:"host_matched"`
	Role                      string                             `json:"role,omitempty"`
	Provider                  string                             `json:"provider,omitempty"`
	Priority                  int                                `json:"priority,omitempty"`
	Status                    string                             `json:"status,omitempty"`
	StatusMessage             string                             `json:"status_message,omitempty"`
	Disabled                  bool                               `json:"disabled,omitempty"`
	Unavailable               bool                               `json:"unavailable,omitempty"`
	NextRetryAfter            time.Time                          `json:"next_retry_after,omitempty"`
	UnknownStatusSeen         int64                              `json:"unknown_status_seen,omitempty"`
	LastUnknownStatusAt       time.Time                          `json:"last_unknown_status_at,omitempty"`
	Eligible                  bool                               `json:"eligible"`
	Reason                    string                             `json:"reason,omitempty"`
	Limits                    map[string]float64                 `json:"limits"`
	ActiveWindows             []string                           `json:"active_windows"`
	RemainingPercent          float64                            `json:"remaining_percent"`
	WindowRemaining           map[string]float64                 `json:"window_remaining"`
	Used                      map[string]float64                 `json:"used"`
	UsedSinceSnapshot         map[string]float64                 `json:"used_since_snapshot,omitempty"`
	QuotaSnapshots            map[string]quotaWindowSnapshot     `json:"quota_snapshots,omitempty"`
	QuotaMode                 string                             `json:"quota_mode"`
	InflightCount             int                                `json:"inflight_count"`
	Success                   int64                              `json:"success,omitempty"`
	Failed                    int64                              `json:"failed,omitempty"`
	RecentRequests            []pluginapi.HostRecentRequestEntry `json:"recent_requests,omitempty"`
	LastUsageAt               time.Time                          `json:"last_usage_at,omitempty"`
	Calibration               map[string]calib                   `json:"calibration,omitempty"`
	LastQueryAt               map[string]time.Time               `json:"last_query_at,omitempty"`
	LastQuotaRefreshAttemptAt time.Time                          `json:"last_quota_refresh_attempt_at,omitempty"`
	LastQuotaRefreshAt        time.Time                          `json:"last_quota_refresh_at,omitempty"`
	LastQuotaRefreshError     string                             `json:"last_quota_refresh_error,omitempty"`
	LastResetAt               map[string]time.Time               `json:"last_reset_at,omitempty"`
	AffinityGroups            []string                           `json:"affinity_groups,omitempty"`
	WeeklyBudget              weeklyBudgetState                  `json:"weekly_budget,omitempty"`
	ProxyStatus               string                             `json:"proxy_status,omitempty"`
	ProxyDisplay              string                             `json:"proxy_display,omitempty"`
	ProxyLastCheckedAt        time.Time                          `json:"proxy_last_checked_at,omitempty"`
	ProxyLastRestoredAt       time.Time                          `json:"proxy_last_restored_at,omitempty"`
	ProxyLastError            string                             `json:"proxy_last_error,omitempty"`
	HealthObservations        []modelHealthObservation           `json:"health_observations,omitempty"`
	ExclusiveOwner            *exclusiveOwnerState               `json:"exclusive_owner,omitempty"`
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
	RebalanceInterval int64                   `json:"rebalance_interval_seconds,omitempty"`
}

type affinityGroupSnapshot struct {
	ID                 string    `json:"id"`
	Members            []string  `json:"members"`
	MainAuthID         string    `json:"main_auth_id,omitempty"`
	BackupAuthIDs      []string  `json:"backup_auth_ids,omitempty"`
	Weight             float64   `json:"weight,omitempty"`
	Source             string    `json:"source,omitempty"`
	CurrentAuthID      string    `json:"current_auth_id,omitempty"`
	CurrentAuthIndex   string    `json:"current_auth_index,omitempty"`
	LastSelectedAt     time.Time `json:"last_selected_at,omitempty"`
	Eligible           bool      `json:"eligible"`
	Reason             string    `json:"reason,omitempty"`
	BindingCount       int       `json:"binding_count,omitempty"`
	ActiveBindingCount int       `json:"active_binding_count,omitempty"`
	Tokens60m          float64   `json:"tokens_60m,omitempty"`
	ActualShare        float64   `json:"actual_share,omitempty"`
	TargetShare        float64   `json:"target_share,omitempty"`
	LoadFactor         float64   `json:"load_factor,omitempty"`
	MainCapacity       float64   `json:"main_capacity,omitempty"`
	FastTokens         float64   `json:"fast_tokens,omitempty"`
	SlowTokens         float64   `json:"slow_tokens,omitempty"`
	OverloadStreak     int       `json:"overload_streak,omitempty"`
	FloorState         string    `json:"floor_state,omitempty"`
	Attribution        string    `json:"attribution,omitempty"`
	APIKeyTokens       float64   `json:"api_key_tokens,omitempty"`
	LocalTokens        float64   `json:"local_tokens,omitempty"`
	UnattributedTokens float64   `json:"unattributed_tokens,omitempty"`
	FiveHourActive     bool      `json:"five_hour_active,omitempty"`
	FiveHourRemaining  float64   `json:"five_hour_remaining_percent,omitempty"`
	WeeklyRemaining    float64   `json:"weekly_remaining_percent,omitempty"`
	ExpectedRemaining  float64   `json:"expected_remaining_percent,omitempty"`
	DailyBurn          float64   `json:"daily_burn_percent,omitempty"`
	ExpectedDailyBurn  float64   `json:"expected_daily_burn_percent,omitempty"`
	BudgetMultiplier   float64   `json:"budget_multiplier,omitempty"`
	BudgetReason       string    `json:"budget_reason,omitempty"`
}

type clientBindingSnapshot struct {
	ClientID            string    `json:"client_id"`
	APIKeyID            string    `json:"api_key_id,omitempty"`
	GroupID             string    `json:"group_id"`
	Source              string    `json:"source,omitempty"`
	CreatedAt           time.Time `json:"created_at,omitempty"`
	UpdatedAt           time.Time `json:"updated_at,omitempty"`
	LastSeenAt          time.Time `json:"last_seen_at,omitempty"`
	LastFailoverAt      time.Time `json:"last_failover_at,omitempty"`
	LastFailoverGroupID string    `json:"last_failover_group_id,omitempty"`
	FailoverCount       int       `json:"failover_count,omitempty"`
	UsageScore60m       float64   `json:"usage_score_60m,omitempty"`
	APIKeyTokens60m     float64   `json:"api_key_tokens_60m,omitempty"`
	UsageSource         string    `json:"usage_source,omitempty"`
	Picks60m            int       `json:"picks_60m,omitempty"`
	HeaderActive60m     bool      `json:"header_active_60m,omitempty"`
	HeaderActiveDormant bool      `json:"header_active_dormant,omitempty"`
	LastHeaderActivity  time.Time `json:"last_header_activity,omitempty"`
	LastAutoMoveAt      time.Time `json:"last_auto_move_at,omitempty"`
	LastManualMoveAt    time.Time `json:"last_manual_move_at,omitempty"`
	LastMoveReason      string    `json:"last_move_reason,omitempty"`
	CooldownUntil       time.Time `json:"cooldown_until,omitempty"`
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
