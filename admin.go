package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
		case "weekly_budget_enabled":
			if v, ok := value.(bool); ok {
				cfg.WeeklyBudgetEnabled = v
			}
		case "weekly_budget_timezone_offset_hours":
			if v, ok := numberValue(value); ok {
				cfg.WeeklyBudgetTimezoneOffsetHours = int(v)
			}
		case "weekly_budget_max_adjustment_percent":
			if v, ok := numberValue(value); ok {
				cfg.WeeklyBudgetMaxAdjustmentPercent = v
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
	lastRefresh := account.LastQuotaRefreshAttemptAt
	if lastRefresh.IsZero() {
		lastRefresh = account.LastQuotaRefreshAt
	}
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
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:24px;color:var(--text-primary);background:var(--bg-primary)}main{max-width:1440px;margin:auto}h1,h2{color:var(--text-primary)}table{width:100%;border-collapse:separate;border-spacing:0;background:var(--bg-secondary);border:1px solid var(--border-color);border-radius:8px;table-layout:auto;overflow:hidden;box-shadow:var(--shadow)}th,td{padding:10px;border-bottom:1px solid var(--border-color);text-align:left;font-size:13px;vertical-align:top}tr:last-child td{border-bottom:0}th{background:var(--bg-tertiary);color:var(--text-secondary);font-weight:600}code,pre{background:var(--bg-tertiary);color:var(--text-primary);border:1px solid var(--border-color);border-radius:4px;padding:2px 4px}pre{white-space:pre-wrap;margin:0}.toolbar,.manual{display:flex;gap:8px;flex-wrap:wrap;margin:16px 0}.section{background:var(--bg-secondary);border:1px solid var(--border-color);border-radius:12px;box-shadow:var(--shadow);margin:16px 0;padding:12px}input,select,button{font:inherit;padding:8px;border:1px solid var(--border-color);border-radius:8px;background:var(--bg-secondary);color:var(--text-primary)}button{background:var(--primary-color);color:var(--primary-contrast);border-color:var(--primary-color);cursor:pointer;font-weight:600}button:hover{background:var(--primary-hover);border-color:var(--primary-hover)}button.secondary{background:var(--bg-tertiary);border-color:var(--border-color);color:var(--text-primary)}button.secondary:hover{background:var(--bg-hover);border-color:var(--border-hover)}.low{color:var(--error-color);font-weight:600}.ok{color:var(--success-color);font-weight:600}.muted{color:var(--text-secondary)}.pill{display:inline-block;border-radius:999px;padding:2px 8px;font-size:12px;background:var(--count-badge-bg);color:var(--count-badge-text);border:1px solid var(--border-color);margin-right:4px}.primary{background:var(--primary-color);color:var(--primary-contrast);border-color:var(--primary-color)}.bad{background:var(--failure-badge-bg);color:var(--failure-badge-text);border-color:var(--failure-badge-border)}.good{background:var(--success-badge-bg);color:var(--success-badge-text);border-color:var(--success-badge-border)}.quota-lines{min-width:145px}.quota-line{display:flex;align-items:baseline;justify-content:space-between;gap:8px;white-space:nowrap;margin-top:3px}.quota-label{font-weight:600}.tight{max-width:180px}.nowrap{white-space:nowrap}.load-cell{min-width:170px;max-width:240px;white-space:normal;line-height:1.45}
</style>`
