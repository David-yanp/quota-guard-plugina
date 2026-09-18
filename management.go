package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
	g.rebuildAffinityGroupsLocked(g.affinityTopologyCandidatesLocked(), g.now())
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
	g.rebuildAffinityGroupsLocked(g.affinityTopologyCandidatesLocked(), g.now())
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
		g.rebuildAffinityGroupsLocked(g.affinityTopologyCandidatesLocked(), now)
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
		APIKeys []struct {
			Key      string  `json:"key"`
			Label    string  `json:"label"`
			Tokens   float64 `json:"tokens"`
			Requests int64   `json:"requests"`
			Share    float64 `json:"share"`
		} `json:"api_keys"`
		Models []struct {
			Tokens   float64 `json:"tokens"`
			Requests int64   `json:"requests"`
		} `json:"models"`
	} `json:"current_usage"`
}
