package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
	g.state.Rebalance.LastAttemptAt = now
	g.saveErr = g.saveStateLocked()
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
	apiKeyUsage := map[string]keeperUsageItem{}
	if cfg.ClientAffinityRebalanceAPIKeyEnabled {
		g.mu.Lock()
		apiKeyIDs := g.clientAPIKeyIDsLocked()
		lastAPIKeyUsageAt := g.state.Rebalance.LastAPIKeyUsageAt
		g.mu.Unlock()
		if forceApply || lastAPIKeyUsageAt.IsZero() || now.Sub(lastAPIKeyUsageAt) >= time.Duration(cfg.ClientAffinityRebalanceAPIKeyRefresh)*time.Second {
			apiKeyUsage = fetchKeeperAPIKeyUsage(cfg.ClientAffinityRebalanceAPIKeyUsageURL, apiKeyIDs, now, cfg.ClientAffinityRebalanceWindowMins)
		}
	}
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
	if len(apiKeyUsage) > 0 || cfg.ClientAffinityRebalanceAPIKeyEnabled {
		if g.state.Rebalance.APIKeyUsage == nil {
			g.state.Rebalance.APIKeyUsage = map[string]keeperUsageItem{}
		}
		for key, usage := range apiKeyUsage {
			g.state.Rebalance.APIKeyUsage[key] = usage
		}
		g.state.Rebalance.LastAPIKeyUsageAt = now
	}
	entry := g.analyzeRebalanceWindowsLocked(fastSnapshot, slowSnapshot, forceApply)
	g.saveErr = g.saveStateLocked()
	return entry, g.saveErr
}

func (g *quotaGuard) clientAPIKeyIDsLocked() []string {
	seen := map[string]bool{}
	for _, binding := range g.state.ClientBindings {
		if binding == nil || strings.TrimSpace(binding.APIKeyID) == "" {
			continue
		}
		seen[strings.TrimSpace(binding.APIKeyID)] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func fetchKeeperAPIKeyUsage(endpoint string, apiKeyIDs []string, now time.Time, windowMinutes int64) map[string]keeperUsageItem {
	result := map[string]keeperUsageItem{}
	for _, apiKeyID := range apiKeyIDs {
		urlValue := strings.ReplaceAll(endpoint, "{api_key_id}", url.QueryEscape(apiKeyID))
		urlValue = keeperUsageURLForWindow(urlValue, windowMinutes)
		item, ok := fetchKeeperAPIKeyUsageItem(urlValue, apiKeyID, now)
		if ok {
			result[apiKeyID] = item
		}
	}
	return result
}

func fetchKeeperAPIKeyUsageItem(endpoint, apiKeyID string, now time.Time) (keeperUsageItem, bool) {
	result, errCall := callHostFunc(pluginabi.MethodHostHTTPDo, pluginapi.HTTPRequest{Method: http.MethodGet, URL: endpoint, Headers: http.Header{"accept": []string{contentTypeJSON}}})
	if errCall != nil {
		return keeperUsageItem{}, false
	}
	var resp pluginapi.HTTPResponse
	if json.Unmarshal(result, &resp) != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return keeperUsageItem{}, false
	}
	var payload keeperRealtimeUsageResponse
	if json.Unmarshal(resp.Body, &payload) != nil || payload.WindowEnd.IsZero() || now.Sub(payload.WindowEnd) > 5*time.Minute || payload.WindowEnd.After(now.Add(5*time.Minute)) {
		return keeperUsageItem{}, false
	}
	var item keeperUsageItem
	for _, candidate := range payload.CurrentUsage.APIKeys {
		if strings.TrimSpace(candidate.Key) == apiKeyID || len(payload.CurrentUsage.APIKeys) == 1 {
			item = keeperUsageItem{AuthIndex: apiKeyID, Label: candidate.Label, Tokens: math.Max(0, candidate.Tokens), Requests: candidate.Requests, Share: candidate.Share}
			return item, true
		}
	}
	for _, model := range payload.CurrentUsage.Models {
		item.Tokens += math.Max(0, model.Tokens)
		item.Requests += model.Requests
	}
	item.AuthIndex = apiKeyID
	return item, item.Tokens > 0 || item.Requests > 0
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
	g.rebuildAffinityGroupsLocked(g.affinityTopologyCandidatesLocked(), now)
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
	g.rebuildAffinityGroupsLocked(g.affinityTopologyCandidatesLocked(), now)
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
	missingBindingFloor := g.hasMissingBindingFloorLocked(loads)
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
	if !streakReady && !missingBindingFloor {
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
	moveReason := "capacity-normalized Keeper load is imbalanced"
	if target.ActiveBindingCount < g.minimumBindingFloorLocked() {
		moveReason = "eligible group is below the minimum binding floor"
	}
	entry := rebalanceHistoryEntry{At: now, Action: "analyze", Result: "recommended", ClientID: candidate.ClientID, FromGroup: source.GroupID, ToGroup: target.GroupID, Reason: moveReason, SourceTokens: source.Tokens, TargetTokens: target.Tokens, SourceLoadFactor: source.LoadFactor, TargetLoadFactor: target.LoadFactor, IdleSeconds: int64(candidate.Idle.Seconds()), EstimatedTokens: candidate.Tokens, ImprovementPercent: improvement}
	requiredImprovement := g.cfg.ClientAffinityRebalanceMinImprove
	if target.ActiveBindingCount < g.minimumBindingFloorLocked() {
		// Filling an eligible group with no binding is a floor repair, not a
		// normal pressure move. Do not let the global spread threshold keep a
		// usable account idle indefinitely.
		requiredImprovement = 0
	} else if target.Tokens <= 0 && source.LoadFactor >= g.cfg.ClientAffinityRebalanceOverload {
		// Empty eligible groups need a chance to warm up. Keep the normal
		// improvement guard for populated targets, but allow a smaller guarded
		// move when the target has received no traffic in the slow window.
		requiredImprovement = math.Min(requiredImprovement, 5)
	}
	if improvement < requiredImprovement {
		entry.Result = "skipped"
		entry.Reason = fmt.Sprintf("predicted improvement %.2f%% is below %.2f%%", improvement, requiredImprovement)
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
	if target.ActiveBindingCount < g.minimumBindingFloorLocked() {
		binding.LastMoveReason = fmt.Sprintf("auto floor repair %.2f%% improvement from %s to %s", improvement, source.GroupID, target.GroupID)
	} else {
		binding.LastMoveReason = fmt.Sprintf("auto rebalance %.2f%% improvement from %s to %s", improvement, source.GroupID, target.GroupID)
	}
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
		budget := main.WeeklyBudget
		_, remaining := g.remainingPercentWithoutInflightLocked(main, now)
		loads[groupID] = groupLoadState{
			GroupID:           groupID,
			Capacity:          capacity,
			Eligible:          eligible,
			Reason:            reason,
			FiveHourActive:    main.ActiveWindows[window5h],
			FiveHourRemaining: remaining[window5h],
			WeeklyRemaining:   remaining[window7d],
			ExpectedRemaining: budget.ExpectedRemaining,
			DailyBurn:         budget.LastDailyBurnPercent,
			ExpectedDailyBurn: budget.ExpectedDailyBurn,
			BudgetMultiplier:  g.weeklyBudgetMultiplierLocked(main),
			BudgetReason:      weeklyBudgetDisplayReason(budget),
		}
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
	type apiKeyRoute struct {
		groupID   string
		authIndex string
	}
	apiKeyRoutes := map[string]map[apiKeyRoute]float64{}
	for _, event := range g.state.ClientActivity {
		if event.Kind != "pick" || event.At.Before(windowStart) || event.APIKeyID == "" || event.AuthID == "" || event.GroupID == "" {
			continue
		}
		account := g.ensureAccountByKeyLocked(event.AuthID)
		if account.AuthIndex == "" {
			continue
		}
		route := apiKeyRoute{groupID: event.GroupID, authIndex: account.AuthIndex}
		if apiKeyRoutes[event.APIKeyID] == nil {
			apiKeyRoutes[event.APIKeyID] = map[apiKeyRoute]float64{}
		}
		apiKeyRoutes[event.APIKeyID][route]++
	}
	knownAPIByAuth := map[string]float64{}
	for apiKeyID, usage := range g.state.Rebalance.APIKeyUsage {
		routes := apiKeyRoutes[apiKeyID]
		if len(routes) != 1 {
			continue
		}
		var route apiKeyRoute
		for candidate := range routes {
			route = candidate
		}
		load, ok := loads[route.groupID]
		if !ok {
			continue
		}
		load.Tokens += usage.Tokens
		load.APIKeyTokens += usage.Tokens
		load.Requests += float64(usage.Requests)
		loads[route.groupID] = load
		knownAPIByAuth[route.authIndex] += usage.Tokens
	}
	for _, event := range g.state.ClientActivity {
		if event.Kind != "usage" || event.GroupID == "" || event.At.Before(windowStart) {
			continue
		}
		load := loads[event.GroupID]
		load.LocalTokens += math.Max(0, event.Score)
		loads[event.GroupID] = load
	}
	for authIndex, usage := range snapshot.AuthFiles {
		groups := normalizeStringList(authGroups[authIndex])
		if len(groups) == 0 {
			continue
		}
		residualTokens := math.Max(0, usage.Tokens-knownAPIByAuth[authIndex])
		if residualTokens <= 0 {
			continue
		}
		if len(groups) == 1 {
			load := loads[groups[0]]
			load.Tokens += residualTokens
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
		if residualTokens > 0 && totalPicks == 0 {
			for _, groupID := range groups {
				load := loads[groupID]
				load.UnattributedTokens += residualTokens / float64(len(groups))
				load.Attribution = "partial"
				loads[groupID] = load
			}
			continue
		}
		for _, groupID := range groups {
			share := 0.0
			if totalPicks > 0 {
				share = pickCounts[groupID] / totalPicks
			}
			load := loads[groupID]
			load.Tokens += residualTokens * share
			load.Requests += float64(usage.Requests) * share
			loads[groupID] = load
		}
	}
	totalTokens := 0.0
	for _, load := range loads {
		totalTokens += load.Tokens
	}
	for groupID, load := range loads {
		if totalTokens > 0 {
			load.ActualShare = load.Tokens / totalTokens * 100
		}
		loads[groupID] = load
	}
	applyTargetShares(loads, totalCapacity, g.cfg)
	g.decorateGroupLoadsLocked(loads)
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
		if totalTokens > 0 {
			load.ActualShare = load.Tokens / totalTokens * 100
		}
		out[id] = load
	}
	applyTargetShares(out, totalCapacity, g.cfg)
	g.decorateGroupLoadsLocked(out)
	return out, nil
}

func applyTargetShares(loads map[string]groupLoadState, totalCapacity float64, cfg pluginConfig) {
	eligible := 0
	for _, load := range loads {
		if load.Eligible && load.Capacity > 0 {
			eligible++
		}
	}
	floor := 0.0
	if cfg.ClientAffinityMinLoadEnabled && eligible > 0 {
		floor = math.Min(cfg.ClientAffinityMinTrafficPercent, 100/float64(eligible))
	}
	remainingShare := math.Max(0, 100-floor*float64(eligible))
	for id, load := range loads {
		if load.Eligible && totalCapacity > 0 {
			load.TargetShare = floor + remainingShare*(load.Capacity/totalCapacity)
			if load.TargetShare > 0 {
				load.LoadFactor = load.ActualShare / load.TargetShare
			}
		}
		loads[id] = load
	}
}

func (g *quotaGuard) decorateGroupLoadsLocked(loads map[string]groupLoadState) {
	dormantSince := g.affinityDormantSinceLocked(g.now())
	for groupID, load := range loads {
		load.BindingCount = g.affinityBindingCountLocked(groupID)
		load.ActiveBindingCount = g.activeAffinityBindingCountLocked(groupID, dormantSince)
		if !load.Eligible {
			load.FloorState = "ineligible"
		} else if load.ActiveBindingCount < g.cfg.ClientAffinityMinBindingPerGroup {
			load.FloorState = "missing active binding"
		} else if load.ActualShare < g.cfg.ClientAffinityMinTrafficPercent {
			load.FloorState = "below traffic floor"
		} else {
			load.FloorState = "satisfied"
		}
		if load.UnattributedTokens > 0 {
			load.Attribution = "partial"
		} else if load.APIKeyTokens > 0 {
			load.Attribution = "keeper api key"
		} else if load.LocalTokens > 0 {
			load.Attribution = "local fallback"
		} else {
			load.Attribution = "keeper auth"
		}
		loads[groupID] = load
	}
}

func (g *quotaGuard) effectiveAffinityCapacityLocked(account *accountState, now time.Time) float64 {
	if account == nil {
		return 1
	}

	_, windowRemaining := g.remainingPercentWithoutInflightLocked(account, now)
	limit := g.effectiveWindowLimitLocked(account, window7d, now)
	remaining := windowRemaining[window7d]
	capacity := math.Max(0, remaining-g.cfg.MinRemainingPercent) / 100 * limit
	if capacity <= 0 {
		base := g.accountAffinityWeightLocked(account, now)
		return math.Max(1, base*0.1)
	}
	return capacity * g.weeklyBudgetMultiplierLocked(account)
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
		if g.clientHasActiveExclusiveLeaseLocked(clientID, now) {
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
	missingBindingFloor := g.hasMissingBindingFloorLocked(loads)
	missingRegularBindingFloor := g.hasMissingRegularBindingFloorLocked(loads)
	minimumBindings := g.minimumBindingFloorLocked()
	bestImprove := -1.0
	bestCandidate := bindingLoadEstimate{}
	bestTarget := groupLoadState{}
	for clientID, binding := range g.state.ClientBindings {
		if binding == nil || binding.LastSeenAt.Before(slowStart) {
			continue
		}
		if g.clientHasActiveExclusiveLeaseLocked(clientID, now) {
			continue
		}
		if !binding.LastAutoMoveAt.IsZero() && now.Sub(binding.LastAutoMoveAt) < autoCooldown {
			continue
		}
		if !binding.LastManualMoveAt.IsZero() && now.Sub(binding.LastManualMoveAt) < manualCooldown {
			continue
		}
		source, okSource := loads[binding.GroupID]
		if !okSource || !source.Eligible || source.ActiveBindingCount <= minimumBindings {
			continue
		}
		floorRepair := missingBindingFloor
		if !floorRepair && (source.LoadFactor < g.cfg.ClientAffinityRebalanceOverload || g.state.Rebalance.OverloadStreak[source.GroupID] < g.cfg.ClientAffinityRebalanceStreak) {
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
			if targetID == source.GroupID || !target.Eligible {
				continue
			}
			targetNeedsBinding := target.ActiveBindingCount < minimumBindings
			// A binding floor is not permission to move traffic into a group that
			// is already under more normalized pressure than the source.
			if target.LoadFactor > source.LoadFactor {
				continue
			}
			if !targetNeedsBinding && target.LoadFactor > g.cfg.ClientAffinityRebalanceTarget {
				continue
			}
			if missingBindingFloor && !targetNeedsBinding {
				continue
			}
			if missingRegularBindingFloor && targetNeedsBinding && g.isReservedAffinityGroupLocked(targetID) {
				continue
			}
			simulated := cloneGroupLoads(loads)
			sourceAfter := simulated[source.GroupID]
			targetAfter := simulated[targetID]
			sourceAfter.Tokens = math.Max(0, sourceAfter.Tokens-estimated)
			targetAfter.Tokens += estimated
			simulated[source.GroupID] = sourceAfter
			simulated[targetID] = targetAfter
			g.recomputeGroupLoadShares(simulated)
			afterMax := maxGroupPressure(simulated)
			// Floor repair may leave the maximum pressure unchanged, but it must
			// never make the global pressure worse.
			if afterMax > beforeMax+1e-9 {
				continue
			}
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
			if targetNeedsBinding && rawImprovement >= 0 {
				// A safe floor repair may have neutral global improvement. It still
				// respects source protection, client quiet time, move cooldowns, and
				// cannot target a more heavily loaded group.
				improvement = math.Max(0, improvement)
			}
			if improvement > bestImprove || (improvement == bestImprove && (clientID < bestCandidate.ClientID || bestCandidate.ClientID == "")) {
				bestImprove = improvement
				bestCandidate = bindingLoadEstimate{ClientID: clientID, Tokens: estimated, Idle: idle}
				bestTarget = target
			}
		}
	}
	return bestCandidate, bestTarget, round2(bestImprove), bestImprove >= 0
}

func (g *quotaGuard) minimumBindingFloorLocked() int {
	if !g.cfg.ClientAffinityMinLoadEnabled || g.cfg.ClientAffinityMinBindingPerGroup <= 0 {
		return 0
	}
	return g.cfg.ClientAffinityMinBindingPerGroup
}

func (g *quotaGuard) hasMissingBindingFloorLocked(loads map[string]groupLoadState) bool {
	minimum := g.minimumBindingFloorLocked()
	if minimum <= 0 {
		return false
	}
	for _, load := range loads {
		if load.Eligible && load.ActiveBindingCount < minimum {
			return true
		}
	}
	return false
}

func (g *quotaGuard) hasMissingRegularBindingFloorLocked(loads map[string]groupLoadState) bool {
	minimum := g.minimumBindingFloorLocked()
	if minimum <= 0 {
		return false
	}
	for groupID, load := range loads {
		if load.Eligible && load.ActiveBindingCount < minimum && !g.isReservedAffinityGroupLocked(groupID) {
			return true
		}
	}
	return false
}

func (g *quotaGuard) isReservedAffinityGroupLocked(groupID string) bool {
	group := g.state.Groups[groupID]
	if group == nil || len(group.Members) != 1 || group.Source == "auto" {
		return false
	}
	account := g.ensureAccountByKeyLocked(firstNonEmpty(group.MainAuthID, firstString(group.Members)))
	return g.accountCanRepeatInAffinityLocked(account, g.now())
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

func (g *quotaGuard) recomputeGroupLoadShares(loads map[string]groupLoadState) {
	totalTokens := 0.0
	totalCapacity := 0.0
	for _, load := range loads {
		totalTokens += load.Tokens
		if load.Eligible {
			totalCapacity += load.Capacity
		}
	}
	for id, load := range loads {
		if totalTokens > 0 {
			load.ActualShare = load.Tokens / totalTokens * 100
		}
		loads[id] = load
	}
	applyTargetShares(loads, totalCapacity, g.cfg)
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
