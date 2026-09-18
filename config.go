package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func newQuotaGuard(now func() time.Time) *quotaGuard {
	return &quotaGuard{
		cfg: defaultConfig(),
		state: stateFile{
			Version:         defaultStateVersion,
			Accounts:        map[string]*accountState{},
			ClientBindings:  map[string]*clientBindingState{},
			ManualGroups:    map[string][]string{},
			Groups:          map[string]*affinityGroupState{},
			GroupCurrent:    map[string]*groupCurrentState{},
			ExclusiveOwners: map[string]*exclusiveOwnerState{},
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
		Version:         defaultStateVersion,
		Accounts:        map[string]*accountState{},
		ClientBindings:  map[string]*clientBindingState{},
		ManualGroups:    map[string][]string{},
		Groups:          map[string]*affinityGroupState{},
		GroupCurrent:    map[string]*groupCurrentState{},
		ExclusiveOwners: map[string]*exclusiveOwnerState{},
	}
	g.loadErr = g.loadStateLocked()
	for clientID, binding := range g.state.ClientBindings {
		if binding == nil || binding.APIKeyID != "" {
			continue
		}
		if apiKeyID := strings.TrimSpace(g.cfg.ClientAffinityAPIKeyMap[clientID]); apiKeyID != "" {
			binding.APIKeyID = apiKeyID
		}
	}
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
	if g.cfg.QuotaRefreshEnabled {
		intervalSecs := g.cfg.QuotaRefreshIntervalSecs
		if intervalSecs <= 0 {
			intervalSecs = 60
		}
		go g.backgroundQuotaRefreshLoop(time.Duration(intervalSecs)*time.Second, g.cfg.QuotaRefreshOnStartup, stop)
	}
	if g.cfg.ClientAffinityRebalanceEnabled {
		intervalSecs := g.cfg.ClientAffinityRebalanceIntervalSecs
		if intervalSecs <= 0 {
			intervalSecs = 300
		}
		go g.backgroundRebalanceLoop(time.Duration(intervalSecs)*time.Second, stop)
	}
	if g.cfg.ProxyRestoreEnabled {
		intervalSecs := g.cfg.ProxyRestoreIntervalSecs
		if intervalSecs <= 0 {
			intervalSecs = 15
		}
		go g.backgroundProxyRestoreLoop(time.Duration(intervalSecs)*time.Second, stop)
	}
}

func (g *quotaGuard) backgroundQuotaRefreshLoop(interval time.Duration, onStartup bool, stop <-chan struct{}) {
	if onStartup {
		g.runBackgroundRefresh()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			g.runBackgroundRefresh()
		case <-stop:
			return
		}
	}
}

func (g *quotaGuard) backgroundRebalanceLoop(interval time.Duration, stop <-chan struct{}) {
	g.runBackgroundRebalance(false)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
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
		if account == nil {
			continue
		}
		if g.quotaResetRefreshDueLocked(account, now) {
			add(refreshRequest{AuthID: account.AuthID, AuthIndex: account.AuthIndex, Force: true})
			continue
		}
		if g.weeklyBudgetSampleDueLocked(account, now) {
			add(refreshRequest{AuthID: account.AuthID, AuthIndex: account.AuthIndex})
		}
	}
	return requests
}

func (g *quotaGuard) weeklyBudgetSampleDueLocked(account *accountState, now time.Time) bool {
	if account == nil || !g.cfg.WeeklyBudgetEnabled {
		return false
	}
	wantDate := weeklyBudgetLocalDate(now, g.cfg.WeeklyBudgetTimezoneOffsetHours)
	samples := account.WeeklyBudget.Samples
	return len(samples) == 0 || samples[len(samples)-1].LocalDate != wantDate
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
	cfg.LegacyPrimaryAuth = strings.TrimSpace(cfg.LegacyPrimaryAuth)
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
	if cfg.ClientAffinityAPIKeyMap == nil {
		cfg.ClientAffinityAPIKeyMap = map[string]string{}
	} else {
		normalizedAPIKeyMap := make(map[string]string, len(cfg.ClientAffinityAPIKeyMap))
		for clientID, apiKeyID := range cfg.ClientAffinityAPIKeyMap {
			clientID = strings.TrimSpace(clientID)
			apiKeyID = strings.TrimSpace(apiKeyID)
			if clientID != "" && apiKeyID != "" {
				normalizedAPIKeyMap[clientID] = apiKeyID
			}
		}
		cfg.ClientAffinityAPIKeyMap = normalizedAPIKeyMap
	}
	if cfg.ClientAffinityExclusiveAuths == nil {
		cfg.ClientAffinityExclusiveAuths = map[string]exclusiveAuthConfig{}
	} else {
		normalizedExclusive := make(map[string]exclusiveAuthConfig, len(cfg.ClientAffinityExclusiveAuths))
		for selector, policy := range cfg.ClientAffinityExclusiveAuths {
			selector = strings.TrimSpace(selector)
			if selector == "" {
				continue
			}
			policy.MaxClients = 1
			if policy.IdleReleaseSeconds <= 0 {
				policy.IdleReleaseSeconds = 3600
			}
			normalizedExclusive[selector] = policy
		}
		cfg.ClientAffinityExclusiveAuths = normalizedExclusive
	}
	if cfg.ClientAffinityMinBindingPerGroup <= 0 {
		cfg.ClientAffinityMinBindingPerGroup = defaults.ClientAffinityMinBindingPerGroup
	}
	if cfg.ClientAffinityMinTrafficPercent <= 0 || cfg.ClientAffinityMinTrafficPercent >= 100 {
		cfg.ClientAffinityMinTrafficPercent = defaults.ClientAffinityMinTrafficPercent
	}
	if cfg.ClientAffinityDormantSecs <= 0 {
		cfg.ClientAffinityDormantSecs = defaults.ClientAffinityDormantSecs
	}
	if strings.TrimSpace(cfg.ClientAffinityRebalanceAPIKeyUsageURL) == "" {
		cfg.ClientAffinityRebalanceAPIKeyUsageURL = defaults.ClientAffinityRebalanceAPIKeyUsageURL
	}
	if cfg.ClientAffinityRebalanceAPIKeyRefresh <= 0 {
		cfg.ClientAffinityRebalanceAPIKeyRefresh = defaults.ClientAffinityRebalanceAPIKeyRefresh
	}
	if strings.TrimSpace(cfg.ClientAffinityUnknownClientPolicy) == "" {
		cfg.ClientAffinityUnknownClientPolicy = defaults.ClientAffinityUnknownClientPolicy
	}
	if strings.TrimSpace(cfg.ClientAffinityUnattributedPolicy) == "" {
		cfg.ClientAffinityUnattributedPolicy = defaults.ClientAffinityUnattributedPolicy
	}
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
	if cfg.WeeklyBudgetTimezoneOffsetHours < -12 || cfg.WeeklyBudgetTimezoneOffsetHours > 14 {
		cfg.WeeklyBudgetTimezoneOffsetHours = defaults.WeeklyBudgetTimezoneOffsetHours
	}
	if cfg.WeeklyBudgetMaxAdjustmentPercent <= 0 || cfg.WeeklyBudgetMaxAdjustmentPercent > 50 {
		cfg.WeeklyBudgetMaxAdjustmentPercent = defaults.WeeklyBudgetMaxAdjustmentPercent
	}
	if cfg.ProxyMapFile == "" {
		cfg.ProxyMapFile = defaults.ProxyMapFile
	}
	if cfg.ProxyRestoreIntervalSecs <= 0 {
		cfg.ProxyRestoreIntervalSecs = defaults.ProxyRestoreIntervalSecs
	}
	if cfg.ProxyRestoreSettleSecs < 0 {
		cfg.ProxyRestoreSettleSecs = defaults.ProxyRestoreSettleSecs
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
