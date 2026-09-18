package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
	account.LastQuotaRefreshAttemptAt = g.now()
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
	g.updateWeeklyBudgetLocked(account, account.QuotaSnapshots[window7d], g.now())
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
	account.LastQuotaRefreshAttemptAt = g.now()
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
	g.updateWeeklyBudgetLocked(account, account.QuotaSnapshots[window7d], g.now())
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
	account.LastQuotaRefreshAttemptAt = g.now()
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
		{names: []string{"five_hour", "fiveHour", "5h"}, window: window5h},
		{names: []string{"weekly", "seven_day", "7d", "week"}, window: window7d},
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
		if !isPrimaryKeeperBucket(bucket) {
			continue
		}
		usedPercent, okUsed := numberValue(bucket["usedPercent"])
		if !okUsed {
			continue
		}
		resetAt := parseTimePtrFromAny(bucket["resetAt"])
		window := windowFromKeeperBucket(bucket, resetAt, now)
		if window != window5h && window != window7d {
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

func isPrimaryKeeperBucket(bucket map[string]any) bool {
	scope := strings.ToLower(strings.TrimSpace(stringFromAny(bucket["scope"])))
	key := strings.ToLower(strings.TrimSpace(stringFromAny(bucket["key"])))
	if scope == "additional" || strings.HasPrefix(key, "additional_rate_limits.") {
		return false
	}
	return scope == "" || scope == "window" || strings.HasPrefix(key, "rate_limit.")
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
	if resetAt != nil && resetAt.After(now.Add(8*24*time.Hour)) {
		return windowMonthly
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
