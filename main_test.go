package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func testGuard(t *testing.T) *quotaGuard {
	t.Helper()
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	g := newQuotaGuard(func() time.Time { return now })
	g.cfg = defaultConfig()
	g.cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	g.cfg.QuotaSnapshotMaxAgeSecs = 900
	return g
}

func candidates(ids ...string) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "any", Priority: 10, Status: "active"})
	}
	return out
}

func affinityPickRequest(clientID string, ids ...string) pluginapi.SchedulerPickRequest {
	headers := http.Header{}
	if clientID != "" {
		headers.Set("X-CPA-Client-ID", clientID)
	}
	return pluginapi.SchedulerPickRequest{
		Options:    pluginapi.SchedulerOptions{Headers: headers},
		Candidates: candidates(ids...),
	}
}

func markAccountsActive(t *testing.T, g *quotaGuard, ids ...string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range ids {
		account := g.ensureAccountByKeyLocked(id)
		account.Status = "active"
	}
}

func TestNoStateSelectsFirstFillFirstCandidate(t *testing.T) {
	g := testGuard(t)
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("AuthID = %q, want a", resp.AuthID)
	}
}

func TestAffinityEnabledWithoutHeaderKeepsLegacyPrimary(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.mu.Lock()
	g.state.CurrentAuthID = "b"
	g.state.CurrentAuthIndex = "idx-b"
	g.mu.Unlock()

	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want legacy primary b without affinity header", resp.AuthID)
	}
	if len(g.state.ClientBindings) != 0 {
		t.Fatalf("client bindings = %#v, want none without affinity header", g.state.ClientBindings)
	}
}

func TestAffinitySameClientSticksToGroupPrimary(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true

	resp, err := g.pick(affinityPickRequest("client-a", "a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("first AuthID = %q, want a", resp.AuthID)
	}
	resp, err = g.pick(affinityPickRequest("client-a", "a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("second AuthID = %q, want group primary a", resp.AuthID)
	}
	binding := g.state.ClientBindings["client-a"]
	if binding == nil || binding.GroupID == "" {
		t.Fatalf("binding = %#v, want persisted group binding", binding)
	}
	if current := g.state.GroupCurrent[binding.GroupID]; current == nil || current.AuthID != "a" {
		t.Fatalf("group current = %#v, want a", current)
	}
}

func TestAffinityGroupPrimaryBelowReserveSwitchesInsideGroup(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	resp, err := g.pick(affinityPickRequest("client-a", "a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("first AuthID = %q, want a", resp.AuthID)
	}
	g.mu.Lock()
	a := g.ensureAccountByKeyLocked("a")
	a.Events = append(a.Events, usageEvent{At: g.now(), Score: 910000})
	g.mu.Unlock()

	resp, err = g.pick(affinityPickRequest("client-a", "a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want b inside bound group after a is below reserve", resp.AuthID)
	}
}

func TestAffinityRebindsWhenBoundGroupUnavailable(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.cfg.ClientAffinityGroups = map[string][]string{
		"group-one": {"a", "b"},
		"group-two": {"c", "d"},
	}
	g.state.ClientBindings["client-a"] = &clientBindingState{ClientID: "client-a", GroupID: "group-one"}
	g.mu.Lock()
	g.ensureAccountByKeyLocked("a").Events = append(g.ensureAccountByKeyLocked("a").Events, usageEvent{At: g.now(), Score: 910000})
	g.ensureAccountByKeyLocked("b").Events = append(g.ensureAccountByKeyLocked("b").Events, usageEvent{At: g.now(), Score: 910000})
	g.mu.Unlock()

	resp, err := g.pick(affinityPickRequest("client-a", "a", "b", "c", "d"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "c" {
		t.Fatalf("AuthID = %q, want c from re-bound group-two", resp.AuthID)
	}
	if got := g.state.ClientBindings["client-a"].GroupID; got != "group-two" {
		t.Fatalf("binding group = %q, want group-two", got)
	}
}

func TestAffinityStateSaveLoadPreservesBindings(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	if _, err := g.pick(affinityPickRequest("client-a", "a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	if err := g.saveStateLocked(); err != nil {
		t.Fatal(err)
	}

	g2 := newQuotaGuard(g.now)
	g2.cfg = g.cfg
	g2.mu.Lock()
	if err := g2.loadStateLocked(); err != nil {
		t.Fatal(err)
	}
	g2.mu.Unlock()
	if binding := g2.state.ClientBindings["client-a"]; binding == nil || binding.GroupID == "" {
		t.Fatalf("loaded binding = %#v, want group binding", binding)
	}
	if len(g2.state.Groups) == 0 || len(g2.state.GroupCurrent) == 0 {
		t.Fatalf("loaded groups/current = %#v / %#v, want preserved affinity state", g2.state.Groups, g2.state.GroupCurrent)
	}
}

func TestAffinityAutoGroupsWeightProIntoMoreGroups(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.mu.Lock()
	pro := g.ensureAccountByKeyLocked("pro")
	pro.ActiveWindows = map[string]bool{window5h: true, window7d: true}
	pro.QuotaSnapshots[window5h] = quotaWindowSnapshot{At: g.now(), Source: "test", RemainingPercent: 99, LimitScore: g.cfg.Default5hLimitScore * g.cfg.ProLimitMultiplier, PlanType: "pro"}
	pro.QuotaSnapshots[window7d] = quotaWindowSnapshot{At: g.now(), Source: "test", RemainingPercent: 99, LimitScore: g.cfg.Default7dLimitScore * g.cfg.ProLimitMultiplier, PlanType: "pro"}
	g.mu.Unlock()

	if _, err := g.pick(affinityPickRequest("client-a", "plus-a", "plus-b", "pro")); err != nil {
		t.Fatal(err)
	}
	snapshot := g.snapshot(false)
	groupCount := map[string]int{}
	for _, account := range snapshot.Accounts {
		groupCount[account.AuthID] = len(account.AffinityGroups)
	}
	if groupCount["pro"] <= groupCount["plus-a"] {
		t.Fatalf("group counts = %#v, want pro in more groups than plus-a", groupCount)
	}
	if groupCount["plus-a"] != 1 || groupCount["plus-b"] != 1 {
		t.Fatalf("group counts = %#v, want regular accounts in exactly one group", groupCount)
	}
}

func TestAffinityAutoGroupsDoNotRepeatRegularAccountsWithoutPro(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true

	if _, err := g.pick(affinityPickRequest("client-a", "plus-a", "plus-b", "team-a", "team-b")); err != nil {
		t.Fatal(err)
	}
	snapshot := g.snapshot(false)
	groupCount := map[string]int{}
	for _, account := range snapshot.Accounts {
		groupCount[account.AuthID] = len(account.AffinityGroups)
	}
	for _, id := range []string{"plus-a", "plus-b", "team-a", "team-b"} {
		if groupCount[id] != 1 {
			t.Fatalf("group counts = %#v, want %s in exactly one group", groupCount, id)
		}
	}
}

func TestManualAffinityGroupFromStateIsUsed(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	files := []pluginapi.HostAuthFileEntry{
		{ID: "a", AuthIndex: "idx-a"},
		{ID: "b", AuthIndex: "idx-b"},
		{ID: "c", AuthIndex: "idx-c"},
	}
	if err := g.saveManualAffinityGroup("manual-a", []string{"idx-b", "idx-c"}, files); err != nil {
		t.Fatal(err)
	}
	resp, err := g.pick(affinityPickRequest("client-a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want first manual group member b", resp.AuthID)
	}
	if got := g.state.ClientBindings["client-a"].GroupID; got != "manual-a" {
		t.Fatalf("binding group = %q, want manual-a", got)
	}
}

func TestManualAffinityStateOverridesConfigGroup(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.cfg.ClientAffinityGroups = map[string][]string{"shared": {"a", "b"}}
	g.mu.Lock()
	g.state.ManualGroups["shared"] = []string{"b", "c"}
	g.mu.Unlock()

	if _, err := g.pick(affinityPickRequest("client-a", "a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	group := g.state.Groups["shared"]
	if group == nil {
		t.Fatal("shared group missing")
	}
	if strings.Join(group.Members, ",") != "b,c" {
		t.Fatalf("members = %#v, want state override b,c", group.Members)
	}
	if group.Source != "manual-state" {
		t.Fatalf("source = %q, want manual-state", group.Source)
	}
}

func TestAffinityKeepsClientBindingsAfterEightHours(t *testing.T) {
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	g := newQuotaGuard(func() time.Time { return now })
	g.cfg = defaultConfig()
	g.cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	g.cfg.ClientAffinityEnabled = true
	g.state.ClientBindings["old"] = &clientBindingState{ClientID: "old", GroupID: "auto-a", LastSeenAt: now.Add(-24 * time.Hour)}
	g.state.ClientBindings["fresh"] = &clientBindingState{ClientID: "fresh", GroupID: "auto-a", LastSeenAt: now.Add(-time.Minute)}

	if _, err := g.pick(affinityPickRequest("new", "a", "b")); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.state.ClientBindings["old"]; !ok {
		t.Fatal("old binding was pruned, want permanent binding")
	}
	if _, ok := g.state.ClientBindings["fresh"]; !ok {
		t.Fatal("fresh binding was pruned")
	}
}

func TestDeleteClientBindingsRemovesOnlySelectedBindings(t *testing.T) {
	g := testGuard(t)
	g.state.ClientBindings["client-a"] = &clientBindingState{ClientID: "client-a", GroupID: "group-a"}
	g.state.ClientBindings["client-b"] = &clientBindingState{ClientID: "client-b", GroupID: "group-b"}
	g.state.Groups["group-a"] = &affinityGroupState{ID: "group-a", Members: []string{"a", "b"}}

	deleted, err := g.deleteClientBindings([]string{"client-a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, ",") != "client-a" {
		t.Fatalf("deleted = %#v, want client-a", deleted)
	}
	if _, ok := g.state.ClientBindings["client-a"]; ok {
		t.Fatal("client-a binding still exists")
	}
	if _, ok := g.state.ClientBindings["client-b"]; !ok {
		t.Fatal("client-b binding was unexpectedly removed")
	}
	if _, ok := g.state.Groups["group-a"]; !ok {
		t.Fatal("group state was unexpectedly removed")
	}
}

func TestMoveClientBindingsMovesOnlySelectedToEligibleGroup(t *testing.T) {
	g := testGuard(t)
	g.state.ClientBindings["client-a"] = &clientBindingState{ClientID: "client-a", GroupID: "group-a"}
	g.state.ClientBindings["client-b"] = &clientBindingState{ClientID: "client-b", GroupID: "group-b"}
	g.state.Groups["group-a"] = &affinityGroupState{ID: "group-a", Members: []string{"a", "b"}}
	g.state.Groups["group-c"] = &affinityGroupState{ID: "group-c", Members: []string{"c", "d"}}
	g.state.GroupCurrent["group-c"] = &groupCurrentState{AuthID: "c", LastSelectedAt: g.now()}
	markAccountsActive(t, g, "a", "b", "c", "d")

	moved, skipped, err := g.moveClientBindings([]string{"client-a"}, "group-c")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(moved, ",") != "client-a" {
		t.Fatalf("moved = %#v, want client-a", moved)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %#v, want none", skipped)
	}
	if got := g.state.ClientBindings["client-a"].GroupID; got != "group-c" {
		t.Fatalf("client-a group = %q, want group-c", got)
	}
	if got := g.state.ClientBindings["client-b"].GroupID; got != "group-b" {
		t.Fatalf("client-b group = %q, want unchanged group-b", got)
	}
	if _, ok := g.state.Groups["group-c"]; !ok {
		t.Fatal("group-c state was unexpectedly removed")
	}
	if current := g.state.GroupCurrent["group-c"]; current == nil || current.AuthID != "c" {
		t.Fatalf("group current = %#v, want preserved", current)
	}
}

func TestMoveClientBindingsSupportsBatchAndSkipsSameGroup(t *testing.T) {
	g := testGuard(t)
	g.state.ClientBindings["client-a"] = &clientBindingState{ClientID: "client-a", GroupID: "group-a"}
	g.state.ClientBindings["client-b"] = &clientBindingState{ClientID: "client-b", GroupID: "group-c"}
	g.state.Groups["group-c"] = &affinityGroupState{ID: "group-c", Members: []string{"c", "d"}}
	markAccountsActive(t, g, "c", "d")

	moved, skipped, err := g.moveClientBindings([]string{"client-a", "client-b"}, "group-c")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(moved, ",") != "client-a" {
		t.Fatalf("moved = %#v, want client-a", moved)
	}
	if strings.Join(skipped, ",") != "client-b" {
		t.Fatalf("skipped = %#v, want client-b", skipped)
	}
	if got := g.state.ClientBindings["client-a"].GroupID; got != "group-c" {
		t.Fatalf("client-a group = %q, want group-c", got)
	}
}

func TestMoveClientBindingsRejectsMissingOrIneligibleTarget(t *testing.T) {
	g := testGuard(t)
	g.state.ClientBindings["client-a"] = &clientBindingState{ClientID: "client-a", GroupID: "group-a"}
	if _, _, err := g.moveClientBindings([]string{"client-a"}, "missing"); err == nil {
		t.Fatal("move to missing group succeeded, want error")
	}

	g.state.Groups["group-low"] = &affinityGroupState{ID: "group-low", Members: []string{"low", "backup"}}
	g.mu.Lock()
	low := g.ensureAccountByKeyLocked("low")
	low.Status = "active"
	low.Events = append(low.Events, usageEvent{At: g.now(), Score: 950000})
	backup := g.ensureAccountByKeyLocked("backup")
	backup.Status = "active"
	backup.Events = append(backup.Events, usageEvent{At: g.now(), Score: 950000})
	g.mu.Unlock()
	if _, _, err := g.moveClientBindings([]string{"client-a"}, "group-low"); err == nil {
		t.Fatal("move to ineligible group succeeded, want error")
	}
}

func TestMoveClientBindingsRejectsEmptyClients(t *testing.T) {
	g := testGuard(t)
	g.state.Groups["group-a"] = &affinityGroupState{ID: "group-a", Members: []string{"a", "b"}}
	if _, _, err := g.moveClientBindings(nil, "group-a"); err == nil {
		t.Fatal("move with empty clients succeeded, want error")
	}
}

func TestMovedClientUsesNewGroupOnNextPick(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.state.ClientBindings["client-a"] = &clientBindingState{ClientID: "client-a", GroupID: "group-a"}
	g.state.ManualGroups["group-a"] = []string{"a", "b"}
	g.state.ManualGroups["group-c"] = []string{"c", "d"}
	markAccountsActive(t, g, "a", "b", "c", "d")
	if _, _, err := g.moveClientBindings([]string{"client-a"}, "group-c"); err != nil {
		t.Fatal(err)
	}

	resp, err := g.pick(affinityPickRequest("client-a", "a", "b", "c", "d"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "c" {
		t.Fatalf("AuthID = %q, want moved group main c", resp.AuthID)
	}
}

func TestAffinityAutoGroupUsesPlusMainBeforeHighPriorityPro(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.mu.Lock()
	pro := g.ensureAccountByKeyLocked("pro")
	pro.ActiveWindows = map[string]bool{window5h: true, window7d: true}
	pro.QuotaSnapshots[window5h] = quotaWindowSnapshot{At: g.now(), Source: "test", RemainingPercent: 99, LimitScore: g.cfg.Default5hLimitScore * g.cfg.ProLimitMultiplier, PlanType: "pro"}
	pro.QuotaSnapshots[window7d] = quotaWindowSnapshot{At: g.now(), Source: "test", RemainingPercent: 99, LimitScore: g.cfg.Default7dLimitScore * g.cfg.ProLimitMultiplier, PlanType: "pro"}
	g.mu.Unlock()

	headers := http.Header{}
	headers.Set("X-CPA-Client-ID", "client-a")
	req := pluginapi.SchedulerPickRequest{
		Options: pluginapi.SchedulerOptions{Headers: headers},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "pro", Provider: "codex", Priority: 100, Status: "active"},
			{ID: "plus-a", Provider: "codex", Priority: 1, Status: "active"},
		},
	}
	resp, err := g.pick(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "plus-a" {
		t.Fatalf("AuthID = %q, want plus-a main before high-priority pro backup", resp.AuthID)
	}
}

func TestAffinityAutoGroupFallsBackToProBackupWhenMainBelowReserve(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.mu.Lock()
	plus := g.ensureAccountByKeyLocked("plus-a")
	plus.Events = append(plus.Events, usageEvent{At: g.now(), Score: 910000})
	pro := g.ensureAccountByKeyLocked("pro")
	pro.ActiveWindows = map[string]bool{window5h: true, window7d: true}
	pro.QuotaSnapshots[window5h] = quotaWindowSnapshot{At: g.now(), Source: "test", RemainingPercent: 99, LimitScore: g.cfg.Default5hLimitScore * g.cfg.ProLimitMultiplier, PlanType: "pro"}
	pro.QuotaSnapshots[window7d] = quotaWindowSnapshot{At: g.now(), Source: "test", RemainingPercent: 99, LimitScore: g.cfg.Default7dLimitScore * g.cfg.ProLimitMultiplier, PlanType: "pro"}
	g.mu.Unlock()

	resp, err := g.pick(affinityPickRequest("client-a", "plus-a", "pro"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "pro" {
		t.Fatalf("AuthID = %q, want pro backup after plus-a is below reserve", resp.AuthID)
	}
}

func TestAffinityMigratesOldAutoBindingToStableAutoGroup(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.mu.Lock()
	g.state.Groups["auto-01"] = &affinityGroupState{ID: "auto-01", Members: []string{"plus-a", "pro"}}
	g.state.ClientBindings["client-a"] = &clientBindingState{ClientID: "client-a", GroupID: "auto-01"}
	pro := g.ensureAccountByKeyLocked("pro")
	pro.ActiveWindows = map[string]bool{window5h: true, window7d: true}
	pro.QuotaSnapshots[window5h] = quotaWindowSnapshot{At: g.now(), Source: "test", RemainingPercent: 99, LimitScore: g.cfg.Default5hLimitScore * g.cfg.ProLimitMultiplier, PlanType: "pro"}
	g.mu.Unlock()

	if _, err := g.pick(affinityPickRequest("client-a", "plus-a", "pro")); err != nil {
		t.Fatal(err)
	}
	if got := g.state.ClientBindings["client-a"].GroupID; got != "auto-plus-a" {
		t.Fatalf("binding group = %q, want auto-plus-a", got)
	}
}

func TestAffinityClearsStaleGroupCurrentWhenMainIsEligible(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.mu.Lock()
	pro := g.ensureAccountByKeyLocked("pro")
	pro.ActiveWindows = map[string]bool{window5h: true, window7d: true}
	pro.QuotaSnapshots[window5h] = quotaWindowSnapshot{At: g.now(), Source: "test", RemainingPercent: 99, LimitScore: g.cfg.Default5hLimitScore * g.cfg.ProLimitMultiplier, PlanType: "pro"}
	g.state.Groups["auto-plus-a"] = &affinityGroupState{
		ID:            "auto-plus-a",
		Members:       []string{"plus-a", "pro"},
		MainAuthID:    "plus-a",
		BackupAuthIDs: []string{"pro"},
	}
	g.state.GroupCurrent["auto-plus-a"] = &groupCurrentState{AuthID: "pro", CurrentRole: "primary", LastSelectedAt: g.now().Add(-time.Hour)}
	g.mu.Unlock()

	_ = g.snapshot(false)
	if current := g.state.GroupCurrent["auto-plus-a"]; current != nil {
		t.Fatalf("group current = %#v, want stale backup current cleared", current)
	}
}

func TestSwitchesWhenCurrentBelowThreshold(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	a := g.ensureAccountByKeyLocked("a")
	a.Events = append(a.Events, usageEvent{At: g.now(), Score: 910000})
	g.mu.Unlock()
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want b", resp.AuthID)
	}
}

func TestStickyCurrentAuthIgnoresTransientInflightReserve(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	a := g.ensureAccountByKeyLocked("a")
	a.Events = append(a.Events, usageEvent{At: g.now(), Score: 800000})
	for range 1 {
		a.Inflight = append(a.Inflight, inflightReserve{At: g.now()})
	}
	g.state.CurrentAuthID = "a"
	g.state.LastSelectedAt = g.now().Add(-time.Minute)
	g.mu.Unlock()

	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("AuthID = %q, want sticky current auth a", resp.AuthID)
	}
}

func TestCurrentPrimaryDoesNotExpireWhileEligible(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	b := g.ensureAccountByKeyLocked("b")
	b.Events = append(b.Events, usageEvent{At: g.now(), Score: 100000})
	g.state.CurrentAuthID = "b"
	g.state.LastSelectedAt = g.now().Add(-time.Duration(g.cfg.StickyCurrentAuthSeconds+1) * time.Second)
	g.mu.Unlock()

	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want current primary b even after old sticky window", resp.AuthID)
	}
}

func TestCurrentPrimaryBelowReserveReselectsByFillFirstOrder(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	b := g.ensureAccountByKeyLocked("b")
	b.Events = append(b.Events, usageEvent{At: g.now(), Score: 910000})
	g.state.CurrentAuthID = "b"
	g.state.LastSelectedAt = g.now().Add(-time.Minute)
	g.mu.Unlock()

	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("AuthID = %q, want a after current primary b falls below reserve", resp.AuthID)
	}
}

func TestStableOrderWithinSamePriority(t *testing.T) {
	g := testGuard(t)
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "b", Priority: 10, Status: "active"},
		{ID: "a", Priority: 10, Status: "active"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("AuthID = %q, want a", resp.AuthID)
	}
}

func TestLowerPriorityDoesNotPreemptHigherPriority(t *testing.T) {
	g := testGuard(t)
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "low", Priority: 1, Status: "active"},
		{ID: "high", Priority: 10, Status: "active"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "high" {
		t.Fatalf("AuthID = %q, want high", resp.AuthID)
	}
}

func TestLegacyFallsBackToLowerPriorityWhenHigherPriorityBelowReserve(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	high := g.ensureAccountByKeyLocked("high")
	high.Events = append(high.Events, usageEvent{At: g.now(), Score: 910000})
	g.state.CurrentAuthID = "high"
	g.mu.Unlock()

	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Priority: 1, Status: "active"},
		{ID: "low", Priority: 0, Status: "active"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "low" {
		t.Fatalf("AuthID = %q, want lower priority fallback", resp.AuthID)
	}
}

func TestDisabledCandidatesAreNotEligible(t *testing.T) {
	g := testGuard(t)
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "a", Priority: 10, Status: "disabled"},
		{ID: "b", Priority: 10, Attributes: map[string]string{"unavailable": "true"}},
		{ID: "c", Priority: 10, Status: "active"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "c" {
		t.Fatalf("AuthID = %q, want c", resp.AuthID)
	}
}

func TestNonActiveStatusIsNotEligible(t *testing.T) {
	g := testGuard(t)
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "a", Priority: 10, Status: "error"},
		{ID: "b", Priority: 9, Status: "active"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want active candidate b", resp.AuthID)
	}
	snapshot := g.snapshot(false)
	for _, account := range snapshot.Accounts {
		if account.AuthID == "a" && (account.Eligible || account.Reason != "status error") {
			t.Fatalf("account a = %#v, want status error rejection", account)
		}
	}
}

func TestRequestScopedErrorStatusCanRemainEligible(t *testing.T) {
	tests := []struct {
		name    string
		message string
	}{
		{
			name:    "context_too_large",
			message: `{"error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error","code":"context_too_large"}}`,
		},
		{
			name:    "context canceled",
			message: "context canceled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := testGuard(t)
			g.mu.Lock()
			account := g.ensureAccountByKeyLocked("a")
			account.Status = "error"
			account.StatusMessage = tt.message
			remaining, windows := g.remainingPercentLocked(account, g.now())
			eligible, reason := g.accountEligibleLocked(account, remaining, windows, g.now())
			g.mu.Unlock()
			if !eligible || reason != "" {
				t.Fatalf("eligible = %v reason=%q, want request-scoped error eligible", eligible, reason)
			}
		})
	}
}

func TestRequestScopedErrorStatusOverrideSelectsCurrentPrimary(t *testing.T) {
	g := testGuard(t)
	originalCallHost := callHostFunc
	t.Cleanup(func() { callHostFunc = originalCallHost })
	callHostFunc = func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthList {
			return nil, nil
		}
		return json.Marshal(authListResponse{Files: []pluginapi.HostAuthFileEntry{{
			ID:            "a",
			AuthIndex:     "idx-a",
			Provider:      "codex",
			Priority:      10,
			Status:        "error",
			StatusMessage: `{"error":{"type":"invalid_request_error","code":"context_too_large"}}`,
		}}})
	}
	g.mu.Lock()
	g.state.CurrentAuthID = "a"
	g.state.CurrentAuthIndex = "idx-a"
	g.mu.Unlock()

	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "a", Provider: "codex", Priority: 10, Status: "error", Attributes: map[string]string{"auth_index": "idx-a"}},
		{ID: "b", Provider: "codex", Priority: 9, Status: "active", Attributes: map[string]string{"auth_index": "idx-b"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("AuthID = %q, want current primary a because error is request-scoped", resp.AuthID)
	}
}

func TestCredentialErrorStatusStillRejected(t *testing.T) {
	tests := []struct {
		name    string
		message string
	}{
		{name: "unauthorized", message: "401 unauthorized"},
		{name: "rate-limit", message: "429 rate limit"},
		{name: "cloudflare", message: "cloudflare challenge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := testGuard(t)
			g.mu.Lock()
			account := g.ensureAccountByKeyLocked("a")
			account.Status = "error"
			account.StatusMessage = tt.message
			remaining, windows := g.remainingPercentLocked(account, g.now())
			eligible, reason := g.accountEligibleLocked(account, remaining, windows, g.now())
			g.mu.Unlock()
			if eligible || reason != "status error" {
				t.Fatalf("eligible = %v reason=%q, want status error rejection", eligible, reason)
			}
		})
	}
}

func TestRequestScopedErrorStatusDoesNotOverrideUnavailableOrCooldown(t *testing.T) {
	tests := []struct {
		name          string
		unavailable   bool
		nextRetry     time.Duration
		wantReasonHas string
	}{
		{name: "unavailable", unavailable: true, wantReasonHas: "unavailable"},
		{name: "cooldown", nextRetry: time.Minute, wantReasonHas: "cooldown until"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := testGuard(t)
			g.mu.Lock()
			account := g.ensureAccountByKeyLocked("a")
			account.Status = "error"
			account.StatusMessage = "context canceled"
			account.Unavailable = tt.unavailable
			if tt.nextRetry > 0 {
				account.NextRetryAfter = g.now().Add(tt.nextRetry)
			}
			remaining, windows := g.remainingPercentLocked(account, g.now())
			eligible, reason := g.accountEligibleLocked(account, remaining, windows, g.now())
			g.mu.Unlock()
			if eligible || !strings.Contains(reason, tt.wantReasonHas) {
				t.Fatalf("eligible = %v reason=%q, want rejection containing %q", eligible, reason, tt.wantReasonHas)
			}
		})
	}
}

func TestEmptyCandidateStatusIsObserved(t *testing.T) {
	g := testGuard(t)
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "a", Provider: "codex", Priority: 10, Attributes: map[string]string{"auth_index": "idx-a"}},
		{ID: "b", Provider: "codex", Priority: 9, Status: "active", Attributes: map[string]string{"auth_index": "idx-b"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want b because empty status is not eligible", resp.AuthID)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	seen := account.UnknownStatusSeen
	last := account.LastUnknownStatusAt
	g.mu.Unlock()
	if seen != 1 {
		t.Fatalf("unknown status seen = %d, want 1", seen)
	}
	if last.IsZero() {
		t.Fatal("last unknown status time was not recorded")
	}
	snapshot := g.snapshot(false)
	found := false
	for _, account := range snapshot.Accounts {
		if account.AuthID == "a" {
			found = true
			if account.UnknownStatusSeen != 1 || account.Reason != "status empty" {
				t.Fatalf("account a = %#v, want unknown status diagnostic", account)
			}
		}
	}
	if !found {
		t.Fatalf("snapshot accounts = %#v, want account a", snapshot.Accounts)
	}
}

func TestActiveWindowsAffectEligibility(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	a := g.ensureAccountByKeyLocked("a")
	a.ActiveWindows[window5h] = true
	a.ActiveWindows[window7d] = true
	a.ActiveWindows[windowMonthly] = true
	a.Events = append(a.Events, usageEvent{At: g.now(), Score: 910000})
	b := g.ensureAccountByKeyLocked("b")
	b.ActiveWindows[windowMonthly] = true
	b.Events = append(b.Events, usageEvent{At: g.now(), Score: 39000000})
	g.mu.Unlock()

	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b", "c")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "c" {
		t.Fatalf("AuthID = %q, want c", resp.AuthID)
	}
}

func TestCalibrationDoesNotFreezeRemaining(t *testing.T) {
	g := testGuard(t)
	g.applyUsage(pluginapi.UsageRecord{AuthID: "a", RequestedAt: g.now(), Detail: pluginapi.UsageDetail{InputTokens: 1000}})
	pct := 50.0
	if err := g.calibrate(calibrateRequest{AuthID: "a", Window: window5h, RemainingPercent: &pct}); err != nil {
		t.Fatal(err)
	}
	g.applyUsage(pluginapi.UsageRecord{AuthID: "a", RequestedAt: g.now(), Detail: pluginapi.UsageDetail{InputTokens: 100}})
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	remaining, _ := g.remainingPercentLocked(account, g.now())
	g.mu.Unlock()
	if remaining >= 50 {
		t.Fatalf("remaining = %.2f, want below calibrated 50", remaining)
	}
}

func TestInflightReserveTemporarilyLowersRemaining(t *testing.T) {
	g := testGuard(t)
	g.cfg.InflightReserveScore = 100000
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("AuthID = %q, want a", resp.AuthID)
	}
	g.mu.Lock()
	remaining, _ := g.remainingPercentLocked(g.ensureAccountByKeyLocked("a"), g.now())
	g.mu.Unlock()
	if remaining != 90 {
		t.Fatalf("remaining = %.2f, want 90", remaining)
	}
}

func TestCodexQuotaSnapshotFeedsRemaining(t *testing.T) {
	g := testGuard(t)
	raw := json.RawMessage(`{
		"codex_quota": {
			"last_refresh_at": "2026-06-22T09:55:00Z",
			"five_hour": {"limit": 100, "remaining": 66, "reset_at": "2026-06-22T14:55:00Z"},
			"weekly": {"limit": 100, "remaining": 93, "reset_at": "2026-06-29T09:55:00Z"}
		}
	}`)
	result := g.applyAuthJSONQuota(pluginapi.HostAuthFileEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex"}, raw, "auth_json")
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	remaining, windows := g.remainingPercentLocked(account, g.now())
	g.mu.Unlock()
	if remaining != 66 {
		t.Fatalf("remaining = %.2f, want 66", remaining)
	}
	if windows[window5h] != 66 || windows[window7d] != 93 {
		t.Fatalf("windows = %#v, want 5h=66 7d=93", windows)
	}
}

func TestFiveHourRemainingDrivesDecisionWhenWeeklyIsLower(t *testing.T) {
	g := testGuard(t)
	raw := json.RawMessage(`{
		"codex_quota": {
			"last_refresh_at": "2026-06-22T09:55:00Z",
			"five_hour": {"limit": 100, "remaining": 86, "reset_at": "2026-06-22T14:55:00Z"},
			"weekly": {"limit": 100, "remaining": 20, "reset_at": "2026-06-29T09:55:00Z"}
		}
	}`)
	result := g.applyAuthJSONQuota(pluginapi.HostAuthFileEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex"}, raw, "auth_json")
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	account.Status = "active"
	remaining, windows := g.remainingPercentLocked(account, g.now())
	eligible, reason := g.accountEligibleLocked(account, remaining, windows, g.now())
	g.mu.Unlock()
	if remaining != 86 {
		t.Fatalf("remaining = %.2f, want 5h-driven 86", remaining)
	}
	if windows[window5h] != 86 || windows[window7d] != 20 {
		t.Fatalf("windows = %#v, want 5h=86 7d=20", windows)
	}
	if !eligible || reason != "" {
		t.Fatalf("eligible = %v reason=%q, want eligible because 5h is above reserve", eligible, reason)
	}
}

func TestWeeklyCapacityMustCoverFiveHourToReserve(t *testing.T) {
	g := testGuard(t)
	raw := json.RawMessage(`{
		"codex_quota": {
			"last_refresh_at": "2026-06-22T09:55:00Z",
			"five_hour": {"limit": 100, "remaining": 99, "reset_at": "2026-06-22T14:55:00Z"},
			"weekly": {"limit": 100, "remaining": 8, "reset_at": "2026-06-29T09:55:00Z"}
		}
	}`)
	result := g.applyAuthJSONQuota(pluginapi.HostAuthFileEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex"}, raw, "auth_json")
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want b because weekly cannot cover 5h-to-reserve", resp.AuthID)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	remaining, windows := g.remainingPercentLocked(account, g.now())
	eligible, reason := g.accountEligibleLocked(account, remaining, windows, g.now())
	g.mu.Unlock()
	if eligible || !strings.Contains(reason, "weekly") {
		t.Fatalf("eligible = %v reason=%q, want weekly capacity rejection", eligible, reason)
	}
}

func TestCodexQuotaLongResetDetectedAsMonthly(t *testing.T) {
	g := testGuard(t)
	raw := json.RawMessage(`{
		"codex_quota": {
			"last_refresh_at": "2026-06-22T09:55:00Z",
			"five_hour": {"limit": 100, "remaining": 93, "reset_at": "2026-07-22T09:55:00Z"},
			"weekly": {"limit": 10000000, "remaining": 10000000}
		}
	}`)
	result := g.applyAuthJSONQuota(pluginapi.HostAuthFileEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex"}, raw, "auth_json")
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	remaining, windows := g.remainingPercentLocked(account, g.now())
	active := activeWindows(account)
	g.mu.Unlock()
	if remaining != 93 {
		t.Fatalf("remaining = %.2f, want 93", remaining)
	}
	if len(active) != 1 || active[0] != windowMonthly {
		t.Fatalf("active windows = %#v, want monthly only", active)
	}
	if windows[windowMonthly] != 93 {
		t.Fatalf("monthly remaining = %.2f, want 93", windows[windowMonthly])
	}
}

func TestUsageAfterQuotaSnapshotIsDiagnosticOnly(t *testing.T) {
	g := testGuard(t)
	g.cfg.InflightReserveScore = 30000
	raw := json.RawMessage(`{"codex_quota":{"last_refresh_at":"2026-06-22T09:55:00Z","five_hour":{"limit":100,"remaining":50},"weekly":{"limit":100,"remaining":80}}}`)
	result := g.applyAuthJSONQuota(pluginapi.HostAuthFileEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex"}, raw, "auth_json")
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	g.applyUsage(pluginapi.UsageRecord{AuthID: "a", RequestedAt: g.now(), Detail: pluginapi.UsageDetail{InputTokens: 100000}})
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	remaining, windows := g.remainingPercentLocked(account, g.now())
	g.mu.Unlock()
	if remaining != 50 {
		t.Fatalf("remaining = %.2f, want authoritative snapshot remaining 50", remaining)
	}
	if windows[window5h] != 50 {
		t.Fatalf("5h remaining = %.2f, want 50", windows[window5h])
	}
	g.mu.Lock()
	account.Inflight = append(account.Inflight, inflightReserve{At: g.now()})
	remaining, windows = g.remainingPercentLocked(account, g.now())
	g.mu.Unlock()
	if remaining != 47 {
		t.Fatalf("remaining = %.2f, want snapshot minus inflight reserve 47", remaining)
	}
	if windows[window5h] != 47 {
		t.Fatalf("5h remaining = %.2f, want 47", windows[window5h])
	}
}

func TestPickRecordsPrimaryInStatus(t *testing.T) {
	g := testGuard(t)
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "a", Priority: 10, Status: "active", Attributes: map[string]string{"auth_index": "idx-a"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "a" {
		t.Fatalf("AuthID = %q, want a", resp.AuthID)
	}
	snapshot := g.snapshot(false)
	if snapshot.CurrentAuthID != "a" || snapshot.CurrentAuthIndex != "idx-a" || snapshot.CurrentRole != "primary" {
		t.Fatalf("current = %#v", snapshot)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Role != "primary" {
		t.Fatalf("accounts = %#v", snapshot.Accounts)
	}
}

func TestSnapshotMarksIneligibleCurrentAsStalePrimary(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	account.Status = "active"
	account.Disabled = true
	g.state.CurrentAuthID = "a"
	g.state.CurrentAuthIndex = "idx-a"
	g.state.CurrentRole = "primary"
	g.state.LastSelectedAt = g.now()
	g.mu.Unlock()

	snapshot := g.snapshot(false)
	if snapshot.CurrentRole != "stale primary" || snapshot.CurrentReason != "disabled" {
		t.Fatalf("current role/reason = %q/%q, want stale primary/disabled", snapshot.CurrentRole, snapshot.CurrentReason)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Role != "last selected" || snapshot.Accounts[0].Eligible {
		t.Fatalf("accounts = %#v, want last selected skipped account", snapshot.Accounts)
	}
}

func TestBackgroundRefreshTargetsCurrentPrimaryOnly(t *testing.T) {
	g := testGuard(t)
	if req, ok := g.backgroundRefreshRequest(); ok {
		t.Fatalf("background refresh request = %#v, want no request before primary selection", req)
	}
	g.mu.Lock()
	g.state.CurrentAuthID = "a"
	g.state.CurrentAuthIndex = "idx-a"
	g.mu.Unlock()
	req, ok := g.backgroundRefreshRequest()
	if !ok {
		t.Fatal("background refresh request was not produced")
	}
	if req.All {
		t.Fatal("All = true, want primary-only refresh")
	}
	if req.AuthID != "a" || req.AuthIndex != "idx-a" {
		t.Fatalf("request = %#v, want auth a idx-a", req)
	}
}

func TestBackgroundRefreshIncludesExpiredQuotaResetAuth(t *testing.T) {
	g := testGuard(t)
	resetAt := g.now().Add(-time.Minute)
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("expired")
	account.AuthID = "expired"
	account.AuthIndex = "idx-expired"
	account.Provider = "codex"
	account.Status = "active"
	account.LastQuotaRefreshAt = resetAt.Add(-time.Minute)
	account.QuotaSnapshots[window5h] = quotaWindowSnapshot{
		At:               resetAt.Add(-5 * time.Hour),
		Source:           "test",
		RemainingPercent: 2,
		LimitScore:       g.cfg.Default5hLimitScore,
		ResetAt:          &resetAt,
	}
	g.mu.Unlock()

	requests := g.backgroundRefreshRequests([]pluginapi.HostAuthFileEntry{{
		ID:        "expired",
		AuthIndex: "idx-expired",
		Provider:  "codex",
	}}, g.now())
	if len(requests) != 1 {
		t.Fatalf("requests = %#v, want one expired reset refresh", requests)
	}
	if requests[0].AuthID != "expired" || requests[0].AuthIndex != "idx-expired" || !requests[0].Force {
		t.Fatalf("request = %#v, want forced expired auth refresh", requests[0])
	}
}

func TestBackgroundRefreshDoesNotRepeatExpiredQuotaResetAfterAttempt(t *testing.T) {
	g := testGuard(t)
	resetAt := g.now().Add(-time.Minute)
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("expired")
	account.AuthID = "expired"
	account.AuthIndex = "idx-expired"
	account.Provider = "codex"
	account.Status = "active"
	account.LastQuotaRefreshAt = g.now()
	account.QuotaSnapshots[window5h] = quotaWindowSnapshot{
		At:               resetAt.Add(-5 * time.Hour),
		Source:           "test",
		RemainingPercent: 2,
		LimitScore:       g.cfg.Default5hLimitScore,
		ResetAt:          &resetAt,
	}
	g.mu.Unlock()

	requests := g.backgroundRefreshRequests([]pluginapi.HostAuthFileEntry{{
		ID:        "expired",
		AuthIndex: "idx-expired",
		Provider:  "codex",
	}}, g.now())
	if len(requests) != 0 {
		t.Fatalf("requests = %#v, want no repeated reset refresh after attempted refresh", requests)
	}
}

func TestBackgroundRefreshKeepsPrimaryAndForcesExpiredReset(t *testing.T) {
	g := testGuard(t)
	resetAt := g.now().Add(-time.Minute)
	g.mu.Lock()
	g.state.CurrentAuthID = "primary"
	g.state.CurrentAuthIndex = "idx-primary"
	expired := g.ensureAccountByKeyLocked("expired")
	expired.AuthID = "expired"
	expired.AuthIndex = "idx-expired"
	expired.Provider = "codex"
	expired.Status = "active"
	expired.LastQuotaRefreshAt = resetAt.Add(-time.Minute)
	expired.QuotaSnapshots[window5h] = quotaWindowSnapshot{
		At:               resetAt.Add(-5 * time.Hour),
		Source:           "test",
		RemainingPercent: 1,
		LimitScore:       g.cfg.Default5hLimitScore,
		ResetAt:          &resetAt,
	}
	g.mu.Unlock()

	requests := g.backgroundRefreshRequests([]pluginapi.HostAuthFileEntry{{
		ID:        "primary",
		AuthIndex: "idx-primary",
		Provider:  "codex",
	}, {
		ID:        "expired",
		AuthIndex: "idx-expired",
		Provider:  "codex",
	}}, g.now())
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want primary plus expired reset refresh", requests)
	}
	if requests[0].AuthID != "primary" || requests[0].Force {
		t.Fatalf("primary request = %#v, want normal primary refresh", requests[0])
	}
	if requests[1].AuthID != "expired" || !requests[1].Force {
		t.Fatalf("expired request = %#v, want forced expired refresh", requests[1])
	}
}

func TestFailedRequestsDefaultIgnoredAndCanBeCounted(t *testing.T) {
	g := testGuard(t)
	g.applyUsage(pluginapi.UsageRecord{AuthID: "a", Failed: true, RequestedAt: g.now(), Detail: pluginapi.UsageDetail{InputTokens: 1000}})
	g.mu.Lock()
	ignored := len(g.ensureAccountByKeyLocked("a").Events)
	g.mu.Unlock()
	if ignored != 0 {
		t.Fatalf("ignored events = %d, want 0", ignored)
	}
	g.cfg.CountFailedRequests = true
	g.applyUsage(pluginapi.UsageRecord{AuthID: "a", Failed: true, RequestedAt: g.now(), Detail: pluginapi.UsageDetail{InputTokens: 1000}})
	g.mu.Lock()
	counted := len(g.ensureAccountByKeyLocked("a").Events)
	g.mu.Unlock()
	if counted != 1 {
		t.Fatalf("counted events = %d, want 1", counted)
	}
}

func TestStateSaveLoadPreservesSelection(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	a := g.ensureAccountByKeyLocked("a")
	a.Events = append(a.Events, usageEvent{At: g.now(), Score: 910000})
	if err := g.saveStateLocked(); err != nil {
		t.Fatal(err)
	}
	g.mu.Unlock()

	g2 := newQuotaGuard(g.now)
	g2.cfg = g.cfg
	g2.mu.Lock()
	if err := g2.loadStateLocked(); err != nil {
		t.Fatal(err)
	}
	g2.mu.Unlock()
	resp, err := g2.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want b", resp.AuthID)
	}
}

func TestPluginRegisterDeclaresCapabilities(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("register failed: %#v", env.Error)
	}
	var reg registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if !reg.Capabilities.Scheduler || !reg.Capabilities.UsagePlugin || !reg.Capabilities.ManagementAPI {
		t.Fatalf("capabilities = %#v", reg.Capabilities)
	}
}

func TestSchedulerPickReturnsValidDelegateWhenDisabled(t *testing.T) {
	g := testGuard(t)
	g.cfg.Enabled = false
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst || !resp.Handled {
		t.Fatalf("response = %#v", resp)
	}
}

func TestManagementRegisterRoutes(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var reg managementRegistration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Routes) != 5 || len(reg.Resources) != 1 {
		t.Fatalf("registration = %#v", reg)
	}
	for _, route := range reg.Routes {
		if strings.Contains(route.Path, "calibrate") {
			t.Fatalf("unexpected calibrate route: %#v", route)
		}
	}
}

func TestManagementHandleAcceptsFullResourcePath(t *testing.T) {
	g := testGuard(t)
	raw, err := g.handleManagement(mustJSON(t, managementRequest{
		Method: "GET",
		Path:   "/v0/resource/plugins/quota-guard/status",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp managementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(resp.Body))
	}
}

func TestDecodeCalibrateRequestAcceptsStringPercent(t *testing.T) {
	body, err := decodeCalibrateRequest([]byte(`{"auth_id":"a","window":"5h","remaining_percent":"72.5"}`))
	if err != nil {
		t.Fatal(err)
	}
	if body.RemainingPercent == nil || *body.RemainingPercent != 72.5 {
		t.Fatalf("remaining_percent = %#v, want 72.5", body.RemainingPercent)
	}
}

func TestRefreshUsesHostHTTPAndAuthJSON(t *testing.T) {
	g := testGuard(t)
	previous := callHostFunc
	t.Cleanup(func() { callHostFunc = previous })
	var refreshedURL string
	callHostFunc = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostHTTPDo:
			raw, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			var req pluginapi.HTTPRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			if req.Method == http.MethodPost {
				if got := req.Headers.Get("X-CPA-Usage-Keeper-Request"); got != "fetch" {
					t.Fatalf("keeper request intent header = %q, want fetch", got)
				}
				resp, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"accepted":1}`)})
				return resp, nil
			}
			refreshedURL = req.URL
			resp, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"status":"ok"}`)})
			return resp, nil
		case pluginabi.MethodHostAuthGet:
			resp, _ := json.Marshal(authGetResponse{
				AuthIndex: "idx-a",
				JSON:      json.RawMessage(`{"codex_quota":{"last_refresh_at":"2026-06-22T09:55:00Z","five_hour":{"limit":100,"remaining":75},"weekly":{"limit":100,"remaining":88}}}`),
			})
			return resp, nil
		default:
			return nil, nil
		}
	}
	g.cfg.QuotaRefreshEndpoint = "http://keeper/quota/{auth_index}"
	results := g.refreshQuotaSnapshots([]pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex"}}, refreshRequest{All: true, Force: true})
	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("results = %#v", results)
	}
	if refreshedURL != "http://keeper/quota/idx-a" {
		t.Fatalf("refreshed URL = %q, want auth index template replacement", refreshedURL)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	remaining, _ := g.remainingPercentLocked(account, g.now())
	g.mu.Unlock()
	if remaining != 75 {
		t.Fatalf("remaining = %.2f, want 75", remaining)
	}
}

func TestResourceRefreshUsesKeeperEndpoint(t *testing.T) {
	g := testGuard(t)
	previous := callHostFunc
	t.Cleanup(func() { callHostFunc = previous })
	httpCalls := 0
	callHostFunc = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			resp, _ := json.Marshal(authListResponse{Files: []pluginapi.HostAuthFileEntry{
				{ID: "a", AuthIndex: "idx-a", Provider: "codex"},
			}})
			return resp, nil
		case pluginabi.MethodHostHTTPDo:
			httpCalls++
			raw, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			var req pluginapi.HTTPRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			if req.Method == http.MethodPost {
				if got := req.Headers.Get("X-CPA-Usage-Keeper-Request"); got != "fetch" {
					t.Fatalf("keeper request intent header = %q, want fetch", got)
				}
				resp, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"accepted":1}`)})
				return resp, nil
			}
			resp, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{
				"authIndex":"idx-a",
				"status":"completed",
				"quota":{"quota":[{"key":"rate_limit.primary_window","usedPercent":25,"window":{"seconds":2628000},"resetAt":"2026-07-22T21:16:52+08:00"}]},
				"refreshed_at":"2026-06-22T17:55:00+08:00"
			}`)})
			return resp, nil
		default:
			return nil, nil
		}
	}
	g.cfg.QuotaRefreshEndpoint = "http://keeper/quota/{auth_index}"
	g.cfg.QuotaRefreshTriggerEndpoint = "http://keeper/quota/refresh"
	g.cfg.QuotaRefreshTriggerWaitSecs = 0
	raw, err := g.handleResourceAction("refresh", url.Values{"auth_index": []string{"idx-a"}, "force": []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %#v", env.Error)
	}
	if httpCalls != 2 {
		t.Fatalf("host.http.do calls = %d, want trigger + read", httpCalls)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	remaining, windows := g.remainingPercentLocked(account, g.now())
	g.mu.Unlock()
	if remaining != 75 || windows[windowMonthly] != 75 {
		t.Fatalf("remaining = %.2f windows=%#v, want monthly 75 from keeper", remaining, windows)
	}
}

func TestKeeperQuotaRefreshResponseFeedsMonthly(t *testing.T) {
	g := testGuard(t)
	raw := []byte(`{
		"authIndex": "idx-a",
		"status": "completed",
		"quota": {
			"id": "idx-a",
			"quota": [{
				"key": "rate_limit.primary_window",
				"label": "Primary",
				"scope": "window",
				"planType": "team",
				"usedPercent": 10,
				"allowed": true,
				"limitReached": false,
				"window": {"seconds": 2628000},
				"resetAt": "2026-07-22T21:16:52+08:00",
				"resetAfterSeconds": 2624419
			}]
		},
		"refreshed_at": "2026-06-22T17:55:00+08:00"
	}`)
	result, ok := g.applyKeeperQuotaRefresh(pluginapi.HostAuthFileEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex"}, raw, "keeper-refresh")
	if !ok {
		t.Fatalf("keeper refresh was not parsed: %#v", result)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	remaining, windows := g.remainingPercentLocked(account, g.now())
	active := activeWindows(account)
	g.mu.Unlock()
	if remaining != 90 {
		t.Fatalf("remaining = %.2f, want 90", remaining)
	}
	if len(active) != 1 || active[0] != windowMonthly {
		t.Fatalf("active windows = %#v, want monthly only", active)
	}
	if windows[windowMonthly] != 90 {
		t.Fatalf("monthly remaining = %.2f, want 90", windows[windowMonthly])
	}
}

func TestKeeperProPlanUsesProLimitMultiplierForUsageDelta(t *testing.T) {
	g := testGuard(t)
	raw := []byte(`{
		"authIndex": "idx-a",
		"status": "completed",
		"quota": {
			"id": "idx-a",
			"quota": [{
				"key": "rate_limit.primary_window",
				"label": "5h",
				"scope": "window",
				"planType": "pro",
				"usedPercent": 0,
				"allowed": true,
				"limitReached": false,
				"window": {"seconds": 18000},
				"resetAt": "2026-06-22T18:55:56+08:00"
			}]
		},
		"refreshed_at": "2026-06-22T10:00:00Z"
	}`)
	result, ok := g.applyKeeperQuotaRefresh(pluginapi.HostAuthFileEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex"}, raw, "keeper-refresh")
	if !ok {
		t.Fatalf("keeper refresh was not parsed: %#v", result)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	account.Events = append(account.Events, usageEvent{At: g.now().Add(time.Minute), Score: 1000000})
	remaining, windows := g.remainingPercentLocked(account, g.now().Add(2*time.Minute))
	snap := account.QuotaSnapshots[window5h]
	g.mu.Unlock()
	if snap.PlanType != "pro" {
		t.Fatalf("plan type = %q, want pro", snap.PlanType)
	}
	if snap.LimitScore != 20000000 {
		t.Fatalf("limit score = %.0f, want 20000000", snap.LimitScore)
	}
	if remaining != 100 || windows[window5h] != 100 {
		t.Fatalf("remaining = %.2f windows=%#v, want authoritative snapshot remaining 100", remaining, windows)
	}
	g.mu.Lock()
	account.Inflight = append(account.Inflight, inflightReserve{At: g.now()})
	remaining, windows = g.remainingPercentLocked(account, g.now().Add(2*time.Minute))
	g.mu.Unlock()
	if remaining != 99.85 || windows[window5h] != 99.85 {
		t.Fatalf("remaining = %.2f windows=%#v, want Pro 20x inflight deduction 99.85", remaining, windows)
	}
}

func TestKeeperPrimaryWindowsOverrideAdditionalModelWindows(t *testing.T) {
	g := testGuard(t)
	raw := []byte(`{
		"authIndex": "idx-a",
		"status": "completed",
		"quota": {
			"id": "idx-a",
			"quota": [
				{
					"key": "rate_limit.primary_window",
					"label": "5h",
					"scope": "window",
					"planType": "pro",
					"usedPercent": 15,
					"window": {"seconds": 18000},
					"resetAt": "2026-06-23T16:39:36+08:00"
				},
				{
					"key": "rate_limit.secondary_window",
					"label": "Weekly",
					"scope": "window",
					"planType": "pro",
					"usedPercent": 11,
					"window": {"seconds": 604800},
					"resetAt": "2026-06-29T14:40:18+08:00"
				},
				{
					"key": "additional_rate_limits.GPT-5.3-Codex-Spark.primary_window",
					"label": "GPT-5.3-Codex-Spark 5h",
					"scope": "additional",
					"metric": "codex_bengalfox",
					"planType": "pro",
					"usedPercent": 0,
					"window": {"seconds": 18000},
					"resetAt": "2026-06-23T21:12:40+08:00"
				},
				{
					"key": "additional_rate_limits.GPT-5.3-Codex-Spark.secondary_window",
					"label": "GPT-5.3-Codex-Spark Weekly",
					"scope": "additional",
					"metric": "codex_bengalfox",
					"planType": "pro",
					"usedPercent": 0,
					"window": {"seconds": 604800},
					"resetAt": "2026-06-29T13:45:15+08:00"
				}
			]
		},
		"refreshed_at": "2026-06-23T08:12:40Z"
	}`)
	result, ok := g.applyKeeperQuotaRefresh(pluginapi.HostAuthFileEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex"}, raw, "keeper-refresh")
	if !ok {
		t.Fatalf("keeper refresh was not parsed: %#v", result)
	}
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	remaining, windows := g.remainingPercentLocked(account, g.now())
	fiveHour := account.QuotaSnapshots[window5h]
	weekly := account.QuotaSnapshots[window7d]
	g.mu.Unlock()
	if remaining != 85 || windows[window5h] != 85 || windows[window7d] != 89 {
		t.Fatalf("remaining = %.2f windows=%#v, want primary 5h=85 weekly=89", remaining, windows)
	}
	if fiveHour.Label != "5h" || weekly.Label != "Weekly" {
		t.Fatalf("labels = %q/%q, want primary labels", fiveHour.Label, weekly.Label)
	}
}

func TestStaleRealQuotaDoesNotFallbackToEstimatedFull(t *testing.T) {
	g := testGuard(t)
	g.cfg.QuotaSnapshotMaxAgeSecs = 60
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	account.ActiveWindows = map[string]bool{window5h: true}
	account.QuotaSnapshots[window5h] = quotaWindowSnapshot{
		At:               g.now().Add(-2 * time.Minute),
		Limit:            100,
		RemainingPercent: 8,
		Source:           "keeper-refresh",
	}
	remaining, _ := g.remainingPercentLocked(account, g.now())
	mode := quotaMode(account, g.now(), g.cfg)
	g.mu.Unlock()
	if remaining != 8 {
		t.Fatalf("remaining = %.2f, want stale real quota remaining 8", remaining)
	}
	if mode != "real+usage stale" {
		t.Fatalf("quota mode = %q, want stale marker", mode)
	}
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != "b" {
		t.Fatalf("AuthID = %q, want b because stale real quota is still below reserve", resp.AuthID)
	}
}

func TestAllLowFailsOrDelegates(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	for _, id := range []string{"a", "b"} {
		account := g.ensureAccountByKeyLocked(id)
		account.Events = append(account.Events, usageEvent{At: g.now(), Score: 910000})
	}
	g.mu.Unlock()
	if _, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")}); err == nil {
		t.Fatal("expected all-low error")
	}
	g.cfg.FailWhenAllLow = false
	resp, err := g.pick(pluginapi.SchedulerPickRequest{Candidates: candidates("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("delegate = %q", resp.DelegateBuiltin)
	}
}

func TestResetWindowDropsRecentEvents(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked("a")
	account.Events = append(account.Events,
		usageEvent{At: g.now().Add(-6 * time.Hour), Score: 1},
		usageEvent{At: g.now().Add(-1 * time.Hour), Score: 1},
	)
	g.mu.Unlock()
	if err := g.resetWindow(resetWindowRequest{AuthID: "a", Window: window5h}); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	got := len(g.ensureAccountByKeyLocked("a").Events)
	g.mu.Unlock()
	if got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
}

func TestResourceCalibrateTargetMustExist(t *testing.T) {
	files := []pluginapi.HostAuthFileEntry{{ID: "auth-a", AuthIndex: "idx-a"}}
	if !resourceCalibrateTargetExists(calibrateRequest{AuthID: "auth-a"}, files) {
		t.Fatal("expected auth_id match")
	}
	if !resourceCalibrateTargetExists(calibrateRequest{AuthIndex: "idx-a"}, files) {
		t.Fatal("expected auth_index match")
	}
	if resourceCalibrateTargetExists(calibrateRequest{AuthID: "missing"}, files) {
		t.Fatal("unexpected missing auth match")
	}
	if resourceCalibrateTargetExists(calibrateRequest{}, files) {
		t.Fatal("unexpected empty target match")
	}
}

func TestDeleteLocalAccountStateOnlyRemovesStaleState(t *testing.T) {
	g := testGuard(t)
	g.mu.Lock()
	g.ensureAccountByKeyLocked("stale-index")
	g.ensureAccountByKeyLocked("real-auth").AuthIndex = "real-index"
	g.mu.Unlock()
	files := []pluginapi.HostAuthFileEntry{{ID: "real-auth", AuthIndex: "real-index"}}
	if err := g.deleteLocalAccountState("real-auth", "", files); err == nil {
		t.Fatal("expected active host auth delete to be refused")
	}
	if err := g.deleteLocalAccountState("stale-index", "", files); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	_, exists := g.state.Accounts["stale-index"]
	g.mu.Unlock()
	if exists {
		t.Fatal("stale state still exists")
	}
}

func TestFetchKeeperUsageSnapshotParsesAuthFiles(t *testing.T) {
	g := testGuard(t)
	previous := callHostFunc
	t.Cleanup(func() { callHostFunc = previous })
	callHostFunc = func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("method = %q", method)
		}
		resp, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"window":        "60m",
			"window_start":  g.now().Add(-time.Hour),
			"window_end":    g.now(),
			"current_usage": map[string]any{"auth_files": []map[string]any{{"key": "idx-a", "tokens": 1234, "requests": 7, "share": 80}}},
		})})
		return resp, nil
	}
	snapshot, err := fetchKeeperUsageSnapshot("http://keeper/usage", g.now())
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.AuthFiles["idx-a"]; got.Tokens != 1234 || got.Requests != 7 {
		t.Fatalf("usage = %#v", got)
	}
}

func TestFetchKeeperUsageSnapshotFailsClosed(t *testing.T) {
	g := testGuard(t)
	previous := callHostFunc
	t.Cleanup(func() { callHostFunc = previous })
	tests := []struct {
		name string
		resp pluginapi.HTTPResponse
	}{
		{name: "forbidden", resp: pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"error":"forbidden"}`)}},
		{name: "stale", resp: pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{"window_end": g.now().Add(-10 * time.Minute), "current_usage": map[string]any{"auth_files": []map[string]any{{"key": "idx-a", "tokens": 1}}}})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callHostFunc = func(string, any) (json.RawMessage, error) {
				return mustJSON(t, tt.resp), nil
			}
			if _, err := fetchKeeperUsageSnapshot("http://keeper/usage", g.now()); err == nil {
				t.Fatal("fetch succeeded, want fail closed")
			}
		})
	}
}

func TestFetchKeeperUsageSnapshotAcceptsEmptyWindow(t *testing.T) {
	g := testGuard(t)
	previous := callHostFunc
	t.Cleanup(func() { callHostFunc = previous })
	callHostFunc = func(string, any) (json.RawMessage, error) {
		resp := pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"window_start":  g.now().Add(-time.Hour),
			"window_end":    g.now(),
			"current_usage": map[string]any{"auth_files": []any{}},
		})}
		return mustJSON(t, resp), nil
	}
	snapshot, err := fetchKeeperUsageSnapshot("http://keeper/usage", g.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.AuthFiles) != 0 {
		t.Fatalf("auth files = %#v", snapshot.AuthFiles)
	}
}

func setupRebalanceGuard(t *testing.T) *quotaGuard {
	t.Helper()
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.cfg.ClientAffinityRebalanceEnabled = true
	g.cfg.ClientAffinityRebalanceMode = "auto"
	g.cfg.ClientAffinityRebalanceWarmupSecs = 0
	g.cfg.ClientAffinityRebalanceIdleSecs = 600
	g.cfg.ClientAffinityRebalanceCooldownSecs = 3600
	g.cfg.ClientAffinityManualCooldownSecs = 86400
	g.cfg.ClientAffinityRebalanceStreak = 1
	g.cfg.ClientAffinityRebalanceOverload = 1.25
	g.cfg.ClientAffinityRebalanceTarget = 0.85
	g.state.Rebalance.StartedAt = g.now().Add(-2 * time.Hour)
	g.state.ManualGroups["group-a"] = []string{"a", "backup-a"}
	g.state.ManualGroups["group-b"] = []string{"b", "backup-b"}
	for id, index := range map[string]string{"a": "idx-a", "backup-a": "idx-backup-a", "b": "idx-b", "backup-b": "idx-backup-b"} {
		account := g.ensureAccountByKeyLocked(id)
		account.AuthIndex = index
		account.Status = "active"
		account.ActiveWindows = map[string]bool{window7d: true}
		account.Limits[window7d] = 100
	}
	return g
}

func rebalanceSnapshot(g *quotaGuard, sourceTokens, targetTokens float64) keeperUsageSnapshot {
	return keeperUsageSnapshot{
		WindowStart: g.now().Add(-time.Hour),
		WindowEnd:   g.now(),
		FetchedAt:   g.now(),
		AuthFiles: map[string]keeperUsageItem{
			"idx-a": {AuthIndex: "idx-a", Tokens: sourceTokens, Requests: 90},
			"idx-b": {AuthIndex: "idx-b", Tokens: targetTokens, Requests: 10},
		},
	}
}

func addRebalanceClient(g *quotaGuard, clientID, groupID string, lastSeen time.Time, score float64) {
	g.state.ClientBindings[clientID] = &clientBindingState{ClientID: clientID, GroupID: groupID, LastSeenAt: lastSeen}
	g.state.ClientActivity = append(g.state.ClientActivity,
		clientActivityEvent{At: g.now().Add(-30 * time.Minute), ClientID: clientID, GroupID: groupID, AuthID: "a", Kind: "pick"},
		clientActivityEvent{At: g.now().Add(-29 * time.Minute), ClientID: clientID, GroupID: groupID, AuthID: "a", Kind: "usage", Score: score},
	)
}

func TestRebalanceMovesOneIdleRecentlyUsedBinding(t *testing.T) {
	g := setupRebalanceGuard(t)
	addRebalanceClient(g, "client-idle", "group-a", g.now().Add(-15*time.Minute), 30)
	addRebalanceClient(g, "client-active", "group-a", g.now().Add(-10*time.Second), 60)
	entry := g.analyzeRebalanceLocked(rebalanceSnapshot(g, 90, 10), false)
	if entry.Result != "moved" || entry.ClientID != "client-idle" {
		t.Fatalf("entry = %#v", entry)
	}
	if got := g.state.ClientBindings["client-idle"].GroupID; got != "group-b" {
		t.Fatalf("group = %q", got)
	}
	if got := g.state.ClientBindings["client-active"].GroupID; got != "group-a" {
		t.Fatalf("active client moved to %q", got)
	}
}

func TestRebalanceEmptyUsageWindowIsSkippedWithoutError(t *testing.T) {
	g := setupRebalanceGuard(t)
	snapshot := keeperUsageSnapshot{WindowStart: g.now().Add(-time.Hour), WindowEnd: g.now(), FetchedAt: g.now(), AuthFiles: map[string]keeperUsageItem{}}
	entry := g.analyzeRebalanceLocked(snapshot, false)
	if entry.Result != "skipped" || entry.Reason != "no usage in analysis window" {
		t.Fatalf("entry = %#v", entry)
	}
	if g.state.Rebalance.LastError != "" {
		t.Fatalf("last error = %q", g.state.Rebalance.LastError)
	}
}

func TestRebalanceLeavesUnusedAndCoolingBindingsUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*clientBindingState, *quotaGuard)
	}{
		{name: "unused", mutate: func(binding *clientBindingState, g *quotaGuard) { binding.LastSeenAt = g.now().Add(-2 * time.Hour) }},
		{name: "manual cooldown", mutate: func(binding *clientBindingState, g *quotaGuard) { binding.LastManualMoveAt = g.now().Add(-time.Hour) }},
		{name: "auto cooldown", mutate: func(binding *clientBindingState, g *quotaGuard) {
			binding.LastAutoMoveAt = g.now().Add(-10 * time.Minute)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := setupRebalanceGuard(t)
			addRebalanceClient(g, "client-idle", "group-a", g.now().Add(-15*time.Minute), 30)
			addRebalanceClient(g, "client-active", "group-a", g.now().Add(-time.Minute), 60)
			tt.mutate(g.state.ClientBindings["client-idle"], g)
			tt.mutate(g.state.ClientBindings["client-active"], g)
			entry := g.analyzeRebalanceLocked(rebalanceSnapshot(g, 90, 10), false)
			if entry.Result == "moved" {
				t.Fatalf("entry = %#v", entry)
			}
			if got := g.state.ClientBindings["client-idle"].GroupID; got != "group-a" {
				t.Fatalf("group = %q", got)
			}
		})
	}
}

func TestRebalanceObserveModeRecordsRecommendationWithoutMoving(t *testing.T) {
	g := setupRebalanceGuard(t)
	g.cfg.ClientAffinityRebalanceMode = "observe"
	addRebalanceClient(g, "client-idle", "group-a", g.now().Add(-15*time.Minute), 30)
	addRebalanceClient(g, "client-active", "group-a", g.now().Add(-time.Minute), 60)
	entry := g.analyzeRebalanceLocked(rebalanceSnapshot(g, 90, 10), false)
	if entry.Result != "recommended" {
		t.Fatalf("entry = %#v", entry)
	}
	if got := g.state.ClientBindings["client-idle"].GroupID; got != "group-a" {
		t.Fatalf("group = %q", got)
	}
}

func TestRebalanceOnceDoesNotBypassWarmup(t *testing.T) {
	g := setupRebalanceGuard(t)
	g.cfg.ClientAffinityRebalanceWarmupSecs = 3600
	g.state.Rebalance.StartedAt = g.now().Add(-10 * time.Minute)
	addRebalanceClient(g, "client-idle", "group-a", g.now().Add(-15*time.Minute), 30)
	addRebalanceClient(g, "client-active", "group-a", g.now().Add(-time.Minute), 60)
	entry := g.analyzeRebalanceLocked(rebalanceSnapshot(g, 90, 10), true)
	if entry.Result != "observed" || !strings.Contains(entry.Reason, "warmup") {
		t.Fatalf("entry = %#v", entry)
	}
	if got := g.state.ClientBindings["client-idle"].GroupID; got != "group-a" {
		t.Fatalf("group = %q", got)
	}
}

func TestRebalanceCombinesFastAndSlowWindows(t *testing.T) {
	g := setupRebalanceGuard(t)
	g.rebuildAffinityGroupsLocked(g.affinitySnapshotCandidatesLocked(), g.now())
	fast := rebalanceSnapshot(g, 50, 0)
	fast.WindowStart = g.now().Add(-5 * time.Minute)
	slow := rebalanceSnapshot(g, 10, 90)
	loads, err := g.buildCompositeGroupLoadsLocked(fast, slow, g.now())
	if err != nil {
		t.Fatal(err)
	}
	load := loads["group-a"]
	if load.FastTokens != 50 || load.SlowTokens != 10 || load.Tokens <= 400 {
		t.Fatalf("load = %#v", load)
	}
}

func TestNormalizeConfigUsesKeeperSupportedFastWindow(t *testing.T) {
	cfg := defaultConfig()
	cfg.ClientAffinityRebalanceFastWindowMins = 5
	cfg = normalizeConfig(cfg)
	if cfg.ClientAffinityRebalanceFastWindowMins != 15 {
		t.Fatalf("fast window = %d, want 15", cfg.ClientAffinityRebalanceFastWindowMins)
	}
}

func TestRebalanceRequiresConsecutiveOverloadSamples(t *testing.T) {
	g := setupRebalanceGuard(t)
	g.cfg.ClientAffinityRebalanceStreak = 3
	addRebalanceClient(g, "client-idle", "group-a", g.now().Add(-15*time.Minute), 30)
	addRebalanceClient(g, "client-active", "group-a", g.now().Add(-10*time.Second), 60)
	for i := 1; i <= 2; i++ {
		entry := g.analyzeRebalanceLocked(rebalanceSnapshot(g, 90, 10), false)
		if entry.Result != "observed" || !strings.Contains(entry.Reason, "consecutive") {
			t.Fatalf("sample %d entry = %#v", i, entry)
		}
	}
	entry := g.analyzeRebalanceLocked(rebalanceSnapshot(g, 90, 10), false)
	if entry.Result != "moved" {
		t.Fatalf("third sample entry = %#v", entry)
	}
}

func TestRebalanceTreatsActiveInflightAsMoveCost(t *testing.T) {
	g := setupRebalanceGuard(t)
	g.cfg.ClientAffinityRebalanceIdleSecs = 30
	addRebalanceClient(g, "client-idle", "group-a", g.now().Add(-time.Minute), 30)
	addRebalanceClient(g, "client-active", "group-a", g.now().Add(-10*time.Second), 60)
	g.ensureAccountByKeyLocked("a").Inflight = append(g.ensureAccountByKeyLocked("a").Inflight, inflightReserve{At: g.now(), ClientID: "client-idle", GroupID: "group-a", AuthID: "a"})
	entry := g.analyzeRebalanceLocked(rebalanceSnapshot(g, 90, 10), false)
	if entry.Result != "moved" {
		t.Fatalf("entry = %#v", entry)
	}
}

func TestEffectiveAffinityCapacityDropsNearReserve(t *testing.T) {
	g := setupRebalanceGuard(t)
	account := g.ensureAccountByKeyLocked("a")
	account.Limits[window7d] = 100
	account.QuotaSnapshots[window7d] = quotaWindowSnapshot{At: g.now(), RemainingPercent: 20, LimitScore: 100}
	low := g.effectiveAffinityCapacityLocked(account, g.now())
	account.QuotaSnapshots[window7d] = quotaWindowSnapshot{At: g.now(), RemainingPercent: 100, LimitScore: 100}
	high := g.effectiveAffinityCapacityLocked(account, g.now())
	if !(low < high && low >= 10) {
		t.Fatalf("low=%.2f high=%.2f", low, high)
	}
}

func TestWeightedRendezvousAssignmentIsStableAndCapacityAware(t *testing.T) {
	g := setupRebalanceGuard(t)
	g.cfg.ClientAffinityRebalanceMode = "auto"
	g.state.ManualGroups = map[string][]string{"large": {"a", "backup-a"}, "small": {"b", "backup-b"}}
	g.ensureAccountByKeyLocked("a").Limits[window7d] = 400
	g.ensureAccountByKeyLocked("b").Limits[window7d] = 100
	cs := candidates("a", "backup-a", "b", "backup-b")
	g.rebuildAffinityGroupsLocked(cs, g.now())
	large := 0
	for i := 0; i < 500; i++ {
		clientID := fmt.Sprintf("client-%d", i)
		group, _, ok := g.assignAffinityGroupLocked(clientID, cs, g.now())
		if !ok {
			t.Fatal("assignment failed")
		}
		repeat, _, _ := g.assignAffinityGroupLocked(clientID, cs, g.now())
		if repeat != group {
			t.Fatalf("unstable assignment %q != %q", group, repeat)
		}
		if group == "large" {
			large++
		}
	}
	if large < 300 {
		t.Fatalf("large assignments = %d, want capacity-weighted majority", large)
	}
}

func TestRebalanceFailsClosedForUnattributedSharedBackup(t *testing.T) {
	g := setupRebalanceGuard(t)
	g.state.ManualGroups["group-a"] = []string{"a", "shared"}
	g.state.ManualGroups["group-b"] = []string{"b", "shared"}
	shared := g.ensureAccountByKeyLocked("shared")
	shared.AuthIndex = "idx-shared"
	shared.Status = "active"
	shared.ActiveWindows = map[string]bool{window7d: true}
	shared.Limits[window7d] = 100
	snapshot := rebalanceSnapshot(g, 90, 10)
	snapshot.AuthFiles["idx-shared"] = keeperUsageItem{AuthIndex: "idx-shared", Tokens: 50, Requests: 5}
	entry := g.analyzeRebalanceLocked(snapshot, false)
	if entry.Result != "error" || !strings.Contains(entry.Reason, "cannot be attributed") {
		t.Fatalf("entry = %#v", entry)
	}
}

func TestRebalanceStatePersistsActivityAndHistory(t *testing.T) {
	g := setupRebalanceGuard(t)
	g.state.ClientActivity = append(g.state.ClientActivity, clientActivityEvent{At: g.now(), ClientID: "client-a", GroupID: "group-a", AuthID: "a", Kind: "pick"})
	g.appendRebalanceHistoryLocked(rebalanceHistoryEntry{At: g.now(), Action: "analyze", Result: "observed", Reason: "test"})
	if err := g.saveStateLocked(); err != nil {
		t.Fatal(err)
	}
	loaded := newQuotaGuard(g.now)
	loaded.cfg = g.cfg
	if err := loaded.loadStateLocked(); err != nil {
		t.Fatal(err)
	}
	if len(loaded.state.ClientActivity) != 1 || len(loaded.state.Rebalance.History) != 1 {
		t.Fatalf("activity=%d history=%d", len(loaded.state.ClientActivity), len(loaded.state.Rebalance.History))
	}
}

func TestClientActivityIsNotRecordedWhenRebalanceDisabled(t *testing.T) {
	g := testGuard(t)
	g.cfg.ClientAffinityEnabled = true
	g.cfg.ClientAffinityRebalanceEnabled = false
	if _, err := g.pick(affinityPickRequest("client-a", "a", "b")); err != nil {
		t.Fatal(err)
	}
	if len(g.state.ClientActivity) != 0 {
		t.Fatalf("activity = %d, want none", len(g.state.ClientActivity))
	}
}

func TestMain(m *testing.M) {
	guard = newQuotaGuard(time.Now)
	os.Exit(m.Run())
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
