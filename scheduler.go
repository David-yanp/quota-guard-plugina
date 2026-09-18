package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
	if identity, ok := g.clientAffinityIdentity(req); ok {
		return g.pickAffinityLocked(candidates, identity.ClientID, identity.APIKeyID, req.Model, now)
	}
	return g.pickLegacyLocked(candidates, req.Model, now)
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

func (g *quotaGuard) pickLegacyLocked(candidates []pluginapi.SchedulerAuthCandidate, model string, now time.Time) (pluginapi.SchedulerPickResponse, error) {
	if preferred, ok := g.preferredLegacyCandidateLocked(candidates, now); ok {
		return g.selectCandidateLocked(preferred, model, now), nil
	}
	if current, ok := g.currentPrimaryCandidateLocked(candidates, now); ok {
		return g.selectCandidateLocked(current, model, now), nil
	}
	for _, candidate := range candidates {
		account := g.ensureAccountLocked(candidate)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		eligible, _ := g.accountSchedulerEligibleForClientLocked(account, remaining, windowRemaining, "", now)
		if eligible {
			return g.selectCandidateLocked(candidate, model, now), nil
		}
	}
	if g.cfg.FailWhenAllLow {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("all %d eligible candidates are below %.2f%% remaining", len(candidates), g.cfg.MinRemainingPercent)
	}
	return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: g.cfg.DelegateWhenUnconfigured}, nil
}

func (g *quotaGuard) preferredLegacyCandidateLocked(candidates []pluginapi.SchedulerAuthCandidate, now time.Time) (pluginapi.SchedulerAuthCandidate, bool) {
	selector := strings.TrimSpace(g.cfg.LegacyPrimaryAuth)
	if selector == "" {
		return pluginapi.SchedulerAuthCandidate{}, false
	}
	for _, candidate := range candidates {
		account := g.ensureAccountLocked(candidate)
		if candidate.ID != selector && account.AuthIndex != selector && strings.TrimSpace(candidate.Attributes["auth_index"]) != selector {
			continue
		}
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		if eligible, _ := g.accountSchedulerEligibleForClientLocked(account, remaining, windowRemaining, "", now); eligible {
			return candidate, true
		}
		return pluginapi.SchedulerAuthCandidate{}, false
	}
	return pluginapi.SchedulerAuthCandidate{}, false
}

type clientAffinityIdentity struct {
	ClientID string
	APIKeyID string
}

func (g *quotaGuard) clientAffinityIdentity(req pluginapi.SchedulerPickRequest) (clientAffinityIdentity, bool) {
	if !g.cfg.ClientAffinityEnabled {
		return clientAffinityIdentity{}, false
	}
	clientID := strings.TrimSpace(http.Header(req.Options.Headers).Get(g.cfg.ClientAffinityHeader))
	if clientID == "" {
		return clientAffinityIdentity{}, false
	}
	apiKeyID := schedulerMetadataString(req.Options.Metadata, "api_key_id", "apiKeyId", "api-key-id")
	if apiKeyID == "" {
		apiKeyID = strings.TrimSpace(g.cfg.ClientAffinityAPIKeyMap[clientID])
	}
	return clientAffinityIdentity{ClientID: clientID, APIKeyID: apiKeyID}, true
}

func schedulerMetadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := metadata[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			return strings.TrimSpace(typed)
		case float64:
			if typed > 0 && typed == math.Trunc(typed) {
				return strconv.FormatInt(int64(typed), 10)
			}
		case json.Number:
			return strings.TrimSpace(typed.String())
		default:
			text := strings.TrimSpace(fmt.Sprint(typed))
			if text != "" && text != "<nil>" {
				return text
			}
		}
	}
	return ""
}

func (g *quotaGuard) pickAffinityLocked(candidates []pluginapi.SchedulerAuthCandidate, clientID, apiKeyID, model string, now time.Time) (pluginapi.SchedulerPickResponse, error) {
	if len(managedAffinityCandidates(candidates)) == 0 {
		return g.pickLegacyLocked(candidates, model, now)
	}
	// Group topology belongs to the complete Codex OAuth account set. The
	// request candidates only determine which members can serve this pick.
	// Rebuilding from a model-specific candidate subset would make groups
	// appear removed and permanently rewrite otherwise valid client bindings.
	g.rebuildAffinityGroupsLocked(g.affinityTopologyCandidatesLocked(), now)
	binding := g.state.ClientBindings[clientID]
	boundGroupID := ""
	if binding != nil {
		boundGroupID = strings.TrimSpace(binding.GroupID)
	}
	_, boundGroupExists := g.state.Groups[boundGroupID]
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
		if boundGroupID != "" && groupID != boundGroupID && !boundGroupExists {
			g.rebindRemovedAffinityGroupLocked(clientID, boundGroupID, groupID, now)
		} else if boundGroupID != "" && groupID != boundGroupID {
			g.recordAffinityFailoverLocked(clientID, boundGroupID, groupID, now)
		}
	}
	if boundGroupID == "" {
		g.upsertClientBindingLocked(clientID, groupID, "client_header", now)
	} else if groupID == boundGroupID {
		g.upsertClientBindingLocked(clientID, groupID, "client_header", now)
	}
	members := g.affinityGroupCandidatesLocked(groupID, candidates)
	if len(members) == 0 {
		return g.pickLegacyLocked(candidates, model, now)
	}
	if selected, ok := g.firstEligibleGroupCandidateLocked(members, clientID, now); ok {
		return g.selectGroupCandidateLocked(groupID, clientID, apiKeyID, model, selected, now), nil
	}
	groupID, reason, ok := g.assignAffinityGroupLocked(clientID, candidates, now)
	if ok {
		if boundGroupID == "" {
			g.upsertClientBindingLocked(clientID, groupID, "client_header", now)
		} else if groupID != boundGroupID && !boundGroupExists {
			g.rebindRemovedAffinityGroupLocked(clientID, boundGroupID, groupID, now)
		} else if groupID != boundGroupID {
			g.recordAffinityFailoverLocked(clientID, boundGroupID, groupID, now)
		} else {
			g.upsertClientBindingLocked(clientID, groupID, "client_header", now)
		}
		members = g.affinityGroupCandidatesLocked(groupID, candidates)
		if selected, selectedOK := g.firstEligibleGroupCandidateLocked(members, clientID, now); selectedOK {
			return g.selectGroupCandidateLocked(groupID, clientID, apiKeyID, model, selected, now), nil
		}
	}
	if g.cfg.FailWhenAllLow {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("all affinity group candidates are below %.2f%% remaining: %s", g.cfg.MinRemainingPercent, reason)
	}
	return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: g.cfg.DelegateWhenUnconfigured}, nil
}

func (g *quotaGuard) recordAffinityFailoverLocked(clientID, fromGroupID, toGroupID string, now time.Time) {
	binding := g.state.ClientBindings[clientID]
	if binding == nil || fromGroupID == "" || toGroupID == "" || fromGroupID == toGroupID {
		return
	}
	binding.LastFailoverAt = now
	binding.LastFailoverGroupID = toGroupID
	binding.FailoverCount++
	binding.LastSeenAt = now
}

// rebindRemovedAffinityGroupLocked handles topology changes, not temporary
// eligibility failures. The replacement remains sticky for future requests.
func (g *quotaGuard) rebindRemovedAffinityGroupLocked(clientID, fromGroupID, toGroupID string, now time.Time) {
	binding := g.state.ClientBindings[clientID]
	if binding == nil || fromGroupID == "" || toGroupID == "" || fromGroupID == toGroupID {
		return
	}
	binding.GroupID = toGroupID
	binding.UpdatedAt = now
	binding.LastSeenAt = now
	binding.LastFailoverAt = now
	binding.LastFailoverGroupID = toGroupID
	binding.FailoverCount++
	binding.LastMoveReason = fmt.Sprintf("topology rebind: %s removed -> %s", fromGroupID, toGroupID)
}

func (g *quotaGuard) firstEligibleGroupCandidateLocked(candidates []pluginapi.SchedulerAuthCandidate, clientID string, now time.Time) (pluginapi.SchedulerAuthCandidate, bool) {
	for _, candidate := range candidates {
		account := g.ensureAccountLocked(candidate)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		eligible, _ := g.accountSchedulerEligibleForClientLocked(account, remaining, windowRemaining, clientID, now)
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
		eligible, _ := g.accountSchedulerEligibleLocked(account, remaining, windowRemaining, now)
		if eligible {
			return candidate, true
		}
		return pluginapi.SchedulerAuthCandidate{}, false
	}
	return pluginapi.SchedulerAuthCandidate{}, false
}

func (g *quotaGuard) selectCandidateLocked(candidate pluginapi.SchedulerAuthCandidate, model string, now time.Time) pluginapi.SchedulerPickResponse {
	account := g.ensureAccountLocked(candidate)
	g.pruneInflightLocked(account, now)
	account.Inflight = append(account.Inflight, inflightReserve{At: now, Model: model, AuthID: candidate.ID})
	g.recordHealthPickLocked(account, model, "", now)
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
		eligible, _ := g.accountSchedulerEligibleLocked(account, remaining, windowRemaining, now)
		if eligible {
			return candidate, true
		}
		return pluginapi.SchedulerAuthCandidate{}, false
	}
	return pluginapi.SchedulerAuthCandidate{}, false
}

func (g *quotaGuard) selectGroupCandidateLocked(groupID, clientID, apiKeyID, model string, candidate pluginapi.SchedulerAuthCandidate, now time.Time) pluginapi.SchedulerPickResponse {
	account := g.ensureAccountLocked(candidate)
	g.pruneInflightLocked(account, now)
	g.touchExclusiveOwnerLocked(account, clientID, now)
	account.Inflight = append(account.Inflight, inflightReserve{At: now, Model: model, ClientID: clientID, APIKeyID: apiKeyID, GroupID: groupID, AuthID: candidate.ID})
	g.recordHealthPickLocked(account, model, apiKeyID, now)
	g.recordClientActivityLocked(clientActivityEvent{At: now, ClientID: clientID, APIKeyID: apiKeyID, GroupID: groupID, AuthID: candidate.ID, Kind: "pick"})
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
	if binding := g.state.ClientBindings[clientID]; binding != nil && apiKeyID != "" {
		binding.APIKeyID = apiKeyID
	}
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
			continue
		}
		// Repeatable accounts may remain in a manual standalone group while
		// also serving as backups for automatically generated groups.
		if g.accountCanRepeatInAffinityLocked(g.ensureAccountLocked(candidate), now) {
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
		selected, ok := g.firstEligibleGroupCandidateLocked(members, "", now)
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
	if account.ActiveWindows[window7d] {
		weight = g.effectiveWindowLimitLocked(account, window7d, now)
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
	if g.affinityGroupHasEligibleLocked(binding.GroupID, candidates, clientID, now) {
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
	dormantSince := g.affinityDormantSinceLocked(now)
	for groupID, group := range g.state.Groups {
		if group == nil {
			continue
		}
		eligible, groupReason := g.affinityGroupEligibleLocked(groupID, candidates, clientID, now)
		if !eligible {
			reason = groupReason
			continue
		}
		if g.cfg.ClientAffinityMinLoadEnabled {
			activeBindingCount := g.activeAffinityBindingCountLocked(groupID, dormantSince)
			if activeBindingCount < g.cfg.ClientAffinityMinBindingPerGroup {
				bestActiveCount := g.activeAffinityBindingCountLocked(bestGroup, dormantSince)
				if bestGroup == "" || activeBindingCount < bestActiveCount || (activeBindingCount == bestActiveCount && groupID < bestGroup) {
					bestGroup = groupID
					bestSet = true
				}
				continue
			}
			load := g.state.Rebalance.Groups[groupID]
			if load.TargetShare > 0 && load.ActualShare < g.cfg.ClientAffinityMinTrafficPercent && load.LoadFactor < g.cfg.ClientAffinityRebalanceOverload {
				if bestGroup == "" || load.ActualShare < g.state.Rebalance.Groups[bestGroup].ActualShare || (load.ActualShare == g.state.Rebalance.Groups[bestGroup].ActualShare && groupID < bestGroup) {
					bestGroup = groupID
					bestSet = true
				}
				continue
			}
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
		score := float64(g.activeAffinityBindingCountLocked(groupID, dormantSince)) / weight
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

func (g *quotaGuard) affinityGroupHasEligibleLocked(groupID string, candidates []pluginapi.SchedulerAuthCandidate, clientID string, now time.Time) bool {
	eligible, _ := g.affinityGroupEligibleLocked(groupID, candidates, clientID, now)
	return eligible
}

func (g *quotaGuard) affinityGroupEligibleLocked(groupID string, candidates []pluginapi.SchedulerAuthCandidate, clientID string, now time.Time) (bool, string) {
	members := g.affinityGroupCandidatesLocked(groupID, candidates)
	if len(members) == 0 {
		return false, "group has no current candidates"
	}
	lastReason := ""
	for _, candidate := range members {
		account := g.ensureAccountLocked(candidate)
		g.pruneInflightLocked(account, now)
		remaining, windowRemaining := g.remainingPercentLocked(account, now)
		eligible, reason := g.accountSchedulerEligibleForClientLocked(account, remaining, windowRemaining, clientID, now)
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

type clientHeaderActivity struct {
	Picks      int
	UsageScore float64
	LastAt     time.Time
}

func (g *quotaGuard) clientHeaderActivityLocked(clientID string, since time.Time) clientHeaderActivity {
	activity := clientHeaderActivity{}
	for _, event := range g.state.ClientActivity {
		if event.ClientID != clientID || event.At.Before(since) {
			continue
		}
		switch event.Kind {
		case "pick":
			activity.Picks++
		case "usage":
			activity.UsageScore += math.Max(0, event.Score)
		default:
			continue
		}
		if event.At.After(activity.LastAt) {
			activity.LastAt = event.At
		}
	}
	return activity
}

func (g *quotaGuard) activeAffinityBindingCountLocked(groupID string, since time.Time) int {
	count := 0
	for clientID, binding := range g.state.ClientBindings {
		if binding == nil || binding.GroupID != groupID {
			continue
		}
		activity := g.clientHeaderActivityLocked(clientID, since)
		if activity.Picks > 0 || activity.UsageScore > 0 || !binding.LastSeenAt.Before(since) {
			count++
		}
	}
	return count
}

func (g *quotaGuard) affinityDormantSinceLocked(now time.Time) time.Time {
	return now.Add(-time.Duration(g.cfg.ClientAffinityDormantSecs) * time.Second)
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
