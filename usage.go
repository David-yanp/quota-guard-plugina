package main

import (
	"encoding/json"
	"log"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
	eventAt := rec.RequestedAt
	if eventAt.IsZero() {
		eventAt = g.now()
	}
	g.recordHealthCompletionLocked(account, reserve, rec, eventAt)
	if rec.Failed && !g.cfg.CountFailedRequests {
		g.saveErr = g.saveStateLocked()
		return
	}
	score := g.scoreUsage(rec)
	if score > 0 {
		account.Events = append(account.Events, usageEvent{At: eventAt, Score: score, Model: rec.Model, Failed: rec.Failed})
		account.LastUsageAt = eventAt
		if reserve.ClientID != "" && reserve.GroupID != "" {
			g.recordClientActivityLocked(clientActivityEvent{At: eventAt, ClientID: reserve.ClientID, APIKeyID: reserve.APIKeyID, GroupID: reserve.GroupID, AuthID: firstNonEmpty(reserve.AuthID, account.AuthID), Kind: "usage", Score: score})
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
	if account.Limits[window7d] <= 0 {
		account.Limits[window7d] = g.cfg.Default7dLimitScore
	}
	if account.Limits[window5h] <= 0 {
		account.Limits[window5h] = g.cfg.Default5hLimitScore
	}
	if account.ActiveWindows == nil {
		account.ActiveWindows = map[string]bool{}
	}
	if account.QuotaSnapshots == nil {
		account.QuotaSnapshots = map[string]quotaWindowSnapshot{}
	}
	delete(account.ActiveWindows, windowMonthly)
	delete(account.QuotaSnapshots, windowMonthly)
	account.ActiveWindows[window7d] = true
	if _, ok := account.QuotaSnapshots[window5h]; ok {
		account.ActiveWindows[window5h] = true
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
	for _, window := range []string{window5h, window7d} {
		if !account.ActiveWindows[window] {
			continue
		}
		active = true
		limit := account.Limits[window]
		if limit <= 0 {
			windowRemaining[window] = 0
			continue
		}
		if snap, ok := quotaSnapshotForSafety(account, window, now, g.cfg); ok {
			snapshotLimit := limit
			if snap.LimitScore > 0 {
				snapshotLimit = snap.LimitScore
			}
			usageFromSnapshot := 0.0
			if !quotaSnapshotFresh(snap, now, g.cfg) {
				usageSinceSnapshot := g.usedScoreSinceLocked(account, window, snap.At, now)
				usageFromSnapshot = usageSinceSnapshot / snapshotLimit * 100
			}
			remainingFromInflight := inflight / snapshotLimit * 100
			remaining := math.Max(0, snap.RemainingPercent-usageFromSnapshot-remainingFromInflight)
			windowRemaining[window] = round2(remaining)
			continue
		}
		used := g.usedScoreLocked(account, window, now)
		remaining := math.Max(0, (limit-used-inflight)/limit*100)
		windowRemaining[window] = round2(remaining)
	}
	if !active {
		return 100, map[string]float64{}
	}
	return primaryRemaining(account, windowRemaining), windowRemaining
}

func quotaSnapshotFresh(snap quotaWindowSnapshot, now time.Time, cfg pluginConfig) bool {
	if snap.At.IsZero() || snap.At.After(now) {
		return false
	}
	if cfg.QuotaSnapshotMaxAgeSecs <= 0 {
		return true
	}
	return now.Sub(snap.At) <= time.Duration(cfg.QuotaSnapshotMaxAgeSecs)*time.Second
}

func primaryRemaining(account *accountState, windowRemaining map[string]float64) float64 {
	if account == nil {
		return 100
	}
	if account.ActiveWindows[window5h] {
		return round2(windowRemaining[window5h])
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
	if cfg.QuotaSnapshotMaxAgeSecs > 0 {
		maxAge := time.Duration(cfg.QuotaSnapshotMaxAgeSecs) * time.Second
		if now.Sub(snap.At) > maxAge {
			return quotaWindowSnapshot{}, false
		}
	}
	return snap, true
}

func quotaSnapshotForSafety(account *accountState, window string, now time.Time, cfg pluginConfig) (quotaWindowSnapshot, bool) {
	if snap, ok := usableQuotaSnapshot(account, window, now, cfg); ok {
		return snap, true
	}
	if account == nil || account.QuotaSnapshots == nil {
		return quotaWindowSnapshot{}, false
	}
	snap, ok := account.QuotaSnapshots[window]
	if !ok || snap.At.IsZero() || snap.RemainingPercent < 0 || snap.At.After(now) {
		return quotaWindowSnapshot{}, false
	}
	// An expired snapshot remains a conservative lower bound until the
	// background refresh succeeds. Local usage since snap.At is still applied.
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
	if reserve.Model != "" {
		observation := g.ensureHealthObservationLocked(account, reserve.Model, reserve.APIKeyID)
		if observation.CurrentInflight > 0 {
			observation.CurrentInflight--
		}
	}
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
	if dormant := time.Duration(g.cfg.ClientAffinityDormantSecs) * time.Second; dormant > retention {
		retention = dormant
	}
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
