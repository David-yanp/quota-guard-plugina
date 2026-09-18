package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
		for _, window := range []string{window5h, window7d} {
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
			AuthID:                    account.AuthID,
			AuthIndex:                 account.AuthIndex,
			HostMatched:               accountMatchesHost(account, hostMatches),
			Role:                      role,
			Provider:                  account.Provider,
			Priority:                  account.Priority,
			Status:                    account.Status,
			StatusMessage:             account.StatusMessage,
			Disabled:                  account.Disabled,
			Unavailable:               account.Unavailable,
			NextRetryAfter:            account.NextRetryAfter,
			UnknownStatusSeen:         account.UnknownStatusSeen,
			LastUnknownStatusAt:       account.LastUnknownStatusAt,
			Eligible:                  eligible,
			Reason:                    reason,
			Limits:                    cloneFloatMap(account.Limits),
			ActiveWindows:             activeWindows(account),
			RemainingPercent:          remaining,
			WindowRemaining:           windowRemaining,
			Used:                      used,
			UsedSinceSnapshot:         usedSinceSnapshot,
			QuotaSnapshots:            cloneQuotaSnapshotMap(account.QuotaSnapshots),
			QuotaMode:                 quotaMode(account, now, g.cfg),
			InflightCount:             len(account.Inflight),
			Success:                   account.Success,
			Failed:                    account.Failed,
			RecentRequests:            append([]pluginapi.HostRecentRequestEntry(nil), account.RecentRequests...),
			LastUsageAt:               account.LastUsageAt,
			Calibration:               cloneCalibMap(account.Calibration),
			LastQueryAt:               cloneTimeMap(account.LastQueryAt),
			LastQuotaRefreshAttemptAt: account.LastQuotaRefreshAttemptAt,
			LastQuotaRefreshAt:        account.LastQuotaRefreshAt,
			LastQuotaRefreshError:     account.LastQuotaRefreshError,
			LastResetAt:               cloneTimeMap(account.LastResetAt),
			AffinityGroups:            g.accountAffinityGroupsLocked(account.AuthID),
			WeeklyBudget:              account.WeeklyBudget,
			ProxyStatus:               account.ProxyStatus,
			ProxyDisplay:              account.ProxyDisplay,
			ProxyLastCheckedAt:        account.ProxyLastCheckedAt,
			ProxyLastRestoredAt:       account.ProxyLastRestoredAt,
			ProxyLastError:            account.ProxyLastError,
			HealthObservations:        g.healthObservationSnapshotLocked(account, now),
			ExclusiveOwner:            cloneExclusiveOwner(g.state.ExclusiveOwners[g.exclusiveOwnerKeyLocked(account)]),
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
		g.rebuildAffinityGroupsLocked(g.affinityTopologyCandidatesLocked(), now)
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
		RebalanceInterval: g.cfg.ClientAffinityRebalanceIntervalSecs,
	}
	groupIDs := make([]string, 0, len(g.state.Groups))
	dormantSince := g.affinityDormantSinceLocked(now)
	windowStart := now.Add(-time.Duration(g.cfg.ClientAffinityRebalanceWindowMins) * time.Minute)
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
		mainAccount := g.ensureAccountByKeyLocked(group.MainAuthID)
		_, currentRemaining := g.remainingPercentWithoutInflightLocked(mainAccount, now)
		load.FiveHourActive = mainAccount.ActiveWindows[window5h]
		load.FiveHourRemaining = currentRemaining[window5h]
		load.WeeklyRemaining = currentRemaining[window7d]
		load.ExpectedRemaining = mainAccount.WeeklyBudget.ExpectedRemaining
		load.DailyBurn = mainAccount.WeeklyBudget.LastDailyBurnPercent
		load.ExpectedDailyBurn = mainAccount.WeeklyBudget.ExpectedDailyBurn
		load.BudgetMultiplier = g.weeklyBudgetMultiplierLocked(mainAccount)
		load.BudgetReason = weeklyBudgetDisplayReason(mainAccount.WeeklyBudget)
		out.Groups = append(out.Groups, affinityGroupSnapshot{
			ID:                 group.ID,
			Members:            append([]string(nil), group.Members...),
			MainAuthID:         group.MainAuthID,
			BackupAuthIDs:      append([]string(nil), group.BackupAuthIDs...),
			Weight:             round2(group.Weight),
			Source:             group.Source,
			CurrentAuthID:      currentAuthID,
			CurrentAuthIndex:   currentAuthIndex,
			LastSelectedAt:     lastSelectedAt,
			Eligible:           eligible,
			Reason:             reason,
			BindingCount:       g.affinityBindingCountLocked(groupID),
			ActiveBindingCount: g.activeAffinityBindingCountLocked(groupID, dormantSince),
			Tokens60m:          round2(load.Tokens),
			ActualShare:        round2(load.ActualShare),
			TargetShare:        round2(load.TargetShare),
			LoadFactor:         round2(load.LoadFactor),
			MainCapacity:       round2(load.Capacity),
			FastTokens:         round2(load.FastTokens),
			SlowTokens:         round2(load.SlowTokens),
			OverloadStreak:     g.state.Rebalance.OverloadStreak[groupID],
			FloorState:         load.FloorState,
			Attribution:        load.Attribution,
			APIKeyTokens:       round2(load.APIKeyTokens),
			LocalTokens:        round2(load.LocalTokens),
			UnattributedTokens: round2(load.UnattributedTokens),
			FiveHourActive:     load.FiveHourActive,
			FiveHourRemaining:  round2(load.FiveHourRemaining),
			WeeklyRemaining:    round2(load.WeeklyRemaining),
			ExpectedRemaining:  round2(load.ExpectedRemaining),
			DailyBurn:          round2(load.DailyBurn),
			ExpectedDailyBurn:  round2(load.ExpectedDailyBurn),
			BudgetMultiplier:   round4(load.BudgetMultiplier),
			BudgetReason:       load.BudgetReason,
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
		apiKeyTokens := 0.0
		activity60m := g.clientHeaderActivityLocked(binding.ClientID, windowStart)
		activityDormant := g.clientHeaderActivityLocked(binding.ClientID, dormantSince)
		if activityDormant.LastAt.IsZero() && !binding.LastSeenAt.IsZero() {
			activityDormant.LastAt = binding.LastSeenAt
		}
		headerActive60m := activity60m.Picks > 0 || activity60m.UsageScore > 0 || !binding.LastSeenAt.Before(windowStart)
		headerActiveDormant := activityDormant.Picks > 0 || activityDormant.UsageScore > 0 || !binding.LastSeenAt.Before(dormantSince)
		usageScore = activity60m.UsageScore
		usageSource := "inactive"
		if binding.APIKeyID != "" {
			if item, ok := g.state.Rebalance.APIKeyUsage[binding.APIKeyID]; ok {
				apiKeyTokens = item.Tokens
				usageSource = "keeper api key"
			}
		}
		if headerActive60m {
			usageSource = "header observed"
		} else if apiKeyTokens > 0 {
			usageSource = "api-key-only"
		} else if headerActiveDormant {
			usageSource = "header dormant"
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
			ClientID:            binding.ClientID,
			APIKeyID:            binding.APIKeyID,
			GroupID:             binding.GroupID,
			Source:              binding.Source,
			CreatedAt:           binding.CreatedAt,
			UpdatedAt:           binding.UpdatedAt,
			LastSeenAt:          binding.LastSeenAt,
			LastFailoverAt:      binding.LastFailoverAt,
			LastFailoverGroupID: binding.LastFailoverGroupID,
			FailoverCount:       binding.FailoverCount,
			UsageScore60m:       round2(usageScore),
			APIKeyTokens60m:     round2(apiKeyTokens),
			UsageSource:         usageSource,
			Picks60m:            activity60m.Picks,
			HeaderActive60m:     headerActive60m,
			HeaderActiveDormant: headerActiveDormant,
			LastHeaderActivity:  activityDormant.LastAt,
			LastAutoMoveAt:      binding.LastAutoMoveAt,
			LastManualMoveAt:    binding.LastManualMoveAt,
			LastMoveReason:      binding.LastMoveReason,
			CooldownUntil:       cooldownUntil,
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

func (g *quotaGuard) affinityTopologyCandidatesLocked() []pluginapi.SchedulerAuthCandidate {
	return managedAffinityCandidates(g.affinitySnapshotCandidatesLocked())
}

func managedAffinityCandidates(candidates []pluginapi.SchedulerAuthCandidate) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if provider != "" && provider != "codex" && provider != "any" {
			continue
		}
		if strings.Contains(strings.ToLower(strings.TrimSpace(candidate.ID)), ":apikey:") {
			continue
		}
		out = append(out, candidate)
	}
	return out
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
	return g.accountEligibleWithStatusPolicyLocked(account, remaining, windowRemaining, now, false)
}

// accountSchedulerEligibleLocked trusts the host candidate set for soft status
// errors. CPA has already applied model-specific disabled, unavailable, and
// cooldown filtering before invoking the scheduler plugin, while an auth's
// aggregate status can still retain an error from another model or an expired
// transient failure.
func (g *quotaGuard) accountSchedulerEligibleLocked(account *accountState, remaining float64, windowRemaining map[string]float64, now time.Time) (bool, string) {
	return g.accountSchedulerEligibleForClientLocked(account, remaining, windowRemaining, "", now)
}

func (g *quotaGuard) accountSchedulerEligibleForClientLocked(account *accountState, remaining float64, windowRemaining map[string]float64, clientID string, now time.Time) (bool, string) {
	if eligible, reason := g.accountEligibleWithStatusPolicyLocked(account, remaining, windowRemaining, now, true); !eligible {
		return false, reason
	}
	return g.exclusiveEligibilityLocked(account, clientID, now)
}

func (g *quotaGuard) accountEligibleWithStatusPolicyLocked(account *accountState, remaining float64, windowRemaining map[string]float64, now time.Time, trustHostCandidate bool) (bool, string) {
	if account.Disabled {
		return false, "disabled"
	}
	if g.cfg.ProxyRestoreBlockMissing && (account.ProxyStatus == "missing" || account.ProxyStatus == "waiting" || account.ProxyStatus == "error") {
		return false, "proxy restore pending: " + firstNonEmpty(account.ProxyStatus, "unknown")
	}
	if account.Unavailable && !g.retryableTransientStatusOverrideLocked(account, now) {
		return false, "unavailable"
	}
	if account.NextRetryAfter.After(now) && !g.retryableTransientStatusOverrideLocked(account, now) {
		return false, "cooldown until " + account.NextRetryAfter.UTC().Format(time.RFC3339)
	}
	switch strings.ToLower(strings.TrimSpace(account.Status)) {
	case "disabled", "unavailable":
		return false, strings.ToLower(strings.TrimSpace(account.Status))
	case "active":
	case "":
		return false, "status empty"
	default:
		if !trustHostCandidate && !g.requestErrorStatusOverrideAllowedLocked(account, now) {
			return false, "status " + strings.ToLower(strings.TrimSpace(account.Status))
		}
	}
	checkedWindow := false
	for _, window := range []string{window5h, window7d} {
		if !account.ActiveWindows[window] {
			continue
		}
		checkedWindow = true
		if windowRemaining[window] < g.cfg.MinRemainingPercent {
			return false, fmt.Sprintf("%s below %.2f%% reserve", quotaWindowLabel(window), g.cfg.MinRemainingPercent)
		}
	}
	if !checkedWindow && remaining < g.cfg.MinRemainingPercent {
		return false, fmt.Sprintf("below %.2f%% reserve", g.cfg.MinRemainingPercent)
	}
	return true, ""
}

func quotaWindowLabel(window string) string {
	switch window {
	case window5h:
		return "5h"
	case window7d:
		return "Weekly"
	default:
		return window
	}
}

func (g *quotaGuard) requestErrorStatusOverrideAllowedLocked(account *accountState, now time.Time) bool {
	if !g.cfg.RequestErrorStatusOverrideEnabled || account == nil {
		return false
	}
	if account.Disabled || (account.Unavailable && !g.retryableTransientStatusOverrideLocked(account, now)) ||
		(account.NextRetryAfter.After(now) && !g.retryableTransientStatusOverrideLocked(account, now)) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(account.Status), "error") {
		return false
	}
	return isRequestScopedStatusMessage(account.StatusMessage)
}

// retryableTransientStatusOverrideLocked keeps a short-lived upstream overload
// from removing an otherwise healthy auth from the plugin's candidate set.
// Credential failures remain governed by the normal unavailable/cooldown rules.
func (g *quotaGuard) retryableTransientStatusOverrideLocked(account *accountState, now time.Time) bool {
	if !g.cfg.RequestErrorStatusOverrideEnabled || account == nil || account.Disabled {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(account.Status), "error") || !isRetryableTransientStatusMessage(account.StatusMessage) {
		return false
	}
	if account.NextRetryAfter.IsZero() {
		return true
	}
	return account.NextRetryAfter.Sub(now) <= 3*time.Minute
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
		"server_is_overloaded",
		"servers are currently overloaded",
	}
	for _, token := range allowed {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

func isRetryableTransientStatusMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(normalized, "server_is_overloaded") ||
		strings.Contains(normalized, "servers are currently overloaded")
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
			if account.LastQuotaRefreshError != "" {
				return "snapshot expired; refresh failed"
			}
			return "real+usage stale"
		}
		if account.LastQuotaRefreshError != "" {
			return "real+usage; last refresh failed"
		}
		return "real+usage"
	}
	if account.LastQuotaRefreshError != "" {
		return "quota unknown; refresh failed"
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
	for _, window := range []string{window5h, window7d} {
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
