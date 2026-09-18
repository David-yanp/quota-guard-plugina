package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	if loaded.ExclusiveOwners == nil {
		loaded.ExclusiveOwners = map[string]*exclusiveOwnerState{}
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
