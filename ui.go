package main

import (
	"bytes"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

func renderStatusPage(status statusResponse) []byte {
	var out bytes.Buffer
	out.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>Quota Guard</title>")
	out.WriteString(quotaGuardThemeScript)
	out.WriteString(quotaGuardStatusStyle)
	out.WriteString("</head><body><main><h1>Quota Guard</h1>")
	out.WriteString("<p class=\"muted\">State file: <code>")
	out.WriteString(html.EscapeString(status.StateFile))
	out.WriteString("</code></p>")
	out.WriteString("<p>Current: ")
	if status.CurrentAuthID != "" {
		pillClass := "pill primary"
		if status.CurrentRole != "" && status.CurrentRole != "primary" {
			pillClass = "pill bad"
		}
		out.WriteString("<span class=\"")
		out.WriteString(pillClass)
		out.WriteString("\">")
		out.WriteString(html.EscapeString(firstNonEmpty(status.CurrentRole, "primary")))
		out.WriteString("</span><code>")
		out.WriteString(html.EscapeString(status.CurrentAuthID))
		out.WriteString("</code>")
		if status.CurrentAuthIndex != "" {
			out.WriteString(" <span class=\"muted\">")
			out.WriteString(html.EscapeString(status.CurrentAuthIndex))
			out.WriteString("</span>")
		}
		if status.CurrentRole != "primary" {
			out.WriteString(" <span class=\"muted\">next pick will reselect")
			if status.CurrentReason != "" {
				out.WriteString(": ")
				out.WriteString(html.EscapeString(status.CurrentReason))
			}
			out.WriteString("</span>")
		}
	} else {
		out.WriteString("<span class=\"muted\">none yet</span>")
	}
	out.WriteString("</p>")
	if status.LoadError != "" || status.SaveError != "" {
		out.WriteString("<pre>")
		out.WriteString(html.EscapeString(strings.TrimSpace(status.LoadError + "\n" + status.SaveError)))
		out.WriteString("</pre>")
	}
	renderAffinitySection(&out, status.Affinity, status.Accounts)
	out.WriteString("<div class=\"toolbar\"><button id=\"quota-guard-refresh-all\">Refresh All</button><button class=\"secondary\" onclick=\"location.reload()\">Reload</button><span id=\"quota-guard-message\" class=\"muted\"></span></div>")
	out.WriteString("<table><thead><tr><th>Role</th><th>Auth</th><th>Host status</th><th>Eligibility</th><th>Quota</th><th>Inflight</th><th>Recent</th><th>Refresh</th><th>Actions</th></tr></thead><tbody>")
	for _, account := range status.Accounts {
		className := "ok"
		if account.RemainingPercent < status.Config.MinRemainingPercent {
			className = "low"
		}
		out.WriteString("<tr><td>")
		if account.Role != "" {
			roleClass := "pill"
			if account.Role == "primary" {
				roleClass = "pill primary"
			}
			out.WriteString("<span class=\"")
			out.WriteString(roleClass)
			out.WriteString("\">")
			out.WriteString(html.EscapeString(account.Role))
			out.WriteString("</span>")
		}
		out.WriteString("</td><td><code>")
		out.WriteString(html.EscapeString(account.AuthID))
		out.WriteString("</code>")
		if account.AuthIndex != "" {
			out.WriteString("<br><span class=\"muted\">")
			out.WriteString(html.EscapeString(account.AuthIndex))
			out.WriteString("</span>")
		}
		out.WriteString("<br><span class=\"muted\">")
		out.WriteString(html.EscapeString(firstNonEmpty(account.Provider, "unknown")))
		out.WriteString(" priority ")
		out.WriteString(strconv.Itoa(account.Priority))
		if len(account.AffinityGroups) > 0 {
			out.WriteString("<br><span class=\"muted\">groups ")
			out.WriteString(html.EscapeString(strings.Join(account.AffinityGroups, ", ")))
			out.WriteString("</span>")
		}
		if account.ExclusiveOwner != nil && account.ExclusiveOwner.ClientID != "" {
			out.WriteString("<br><span class=\"muted\">exclusive owner ")
			out.WriteString(html.EscapeString(account.ExclusiveOwner.ClientID))
			out.WriteString(" · lease until ")
			out.WriteString(html.EscapeString(account.ExclusiveOwner.LeaseExpiresAt.Format("01-02 15:04")))
			out.WriteString("</span>")
		}
		out.WriteString("</span></td><td>")
		out.WriteString(html.EscapeString(firstNonEmpty(account.Status, "unknown")))
		if account.Disabled {
			out.WriteString(" <span class=\"pill bad\">disabled</span>")
		}
		if account.Unavailable {
			out.WriteString(" <span class=\"pill bad\">unavailable</span>")
		}
		if !account.NextRetryAfter.IsZero() {
			out.WriteString("<br><span class=\"muted\">retry after ")
			out.WriteString(html.EscapeString(account.NextRetryAfter.Format("01-02 15:04")))
			out.WriteString("</span>")
		}
		if account.StatusMessage != "" {
			out.WriteString("<br><span class=\"muted\">")
			out.WriteString(html.EscapeString(account.StatusMessage))
			out.WriteString("</span>")
		}
		if account.UnknownStatusSeen > 0 {
			out.WriteString("<br><span class=\"muted\">unknown status seen ")
			out.WriteString(strconv.FormatInt(account.UnknownStatusSeen, 10))
			if !account.LastUnknownStatusAt.IsZero() {
				out.WriteString(" · last ")
				out.WriteString(html.EscapeString(account.LastUnknownStatusAt.Format("01-02 15:04")))
			}
			out.WriteString("</span>")
		}
		out.WriteString("</td><td>")
		if account.Eligible {
			out.WriteString("<span class=\"pill good\">eligible</span>")
		} else {
			out.WriteString("<span class=\"pill bad\">skipped</span>")
		}
		if account.Reason != "" {
			out.WriteString("<br><span class=\"muted\">")
			out.WriteString(html.EscapeString(account.Reason))
			out.WriteString("</span>")
		}
		out.WriteString("</td><td><span class=\"")
		out.WriteString(className)
		out.WriteString("\">")
		out.WriteString(fmt.Sprintf("%.2f%%", account.RemainingPercent))
		out.WriteString("</span><br><span class=\"muted\">")
		out.WriteString(html.EscapeString(account.QuotaMode))
		out.WriteString(" · ")
		out.WriteString(html.EscapeString(strings.Join(account.ActiveWindows, ", ")))
		out.WriteString("</span>")
		renderQuotaLines(&out, account)
		if account.ProxyStatus != "" {
			out.WriteString("<br><span class=\"muted\">proxy ")
			out.WriteString(html.EscapeString(account.ProxyStatus))
			if account.ProxyDisplay != "" {
				out.WriteString(" · ")
				out.WriteString(html.EscapeString(account.ProxyDisplay))
			}
			out.WriteString("</span>")
		}
		out.WriteString("</td><td class=\"nowrap\">")
		out.WriteString(strconv.Itoa(account.InflightCount))
		out.WriteString("<br><span class=\"muted\">guard reserve ")
		out.WriteString(fmt.Sprintf("%.2f%%", status.Config.MinRemainingPercent))
		out.WriteString("</span>")
		if account.InflightCount > 0 && status.Config.InflightReserveScore > 0 {
			out.WriteString("<br><span class=\"muted\">inflight score ")
			out.WriteString(formatScore(float64(account.InflightCount) * status.Config.InflightReserveScore))
			out.WriteString("</span>")
		}
		out.WriteString("</td><td class=\"tight\">")
		out.WriteString(html.EscapeString(recentSummary(account)))
		renderHealthObservations(&out, account.HealthObservations, status.GeneratedAt)
		out.WriteString("</td><td>")
		if !account.LastQuotaRefreshAt.IsZero() {
			out.WriteString("snapshot ")
			out.WriteString(html.EscapeString(account.LastQuotaRefreshAt.Format(time.RFC3339)))
		}
		if !account.LastQuotaRefreshAttemptAt.IsZero() {
			out.WriteString("<br><span class=\"muted\">attempt ")
			out.WriteString(html.EscapeString(account.LastQuotaRefreshAttemptAt.Format(time.RFC3339)))
			out.WriteString("</span>")
		}
		if account.LastQuotaRefreshError != "" {
			out.WriteString("<br><span class=\"low\">refresh failed: ")
			out.WriteString(html.EscapeString(account.LastQuotaRefreshError))
			out.WriteString("</span>")
		}
		out.WriteString("</td><td>")
		out.WriteString("<button data-refresh=\"")
		out.WriteString(html.EscapeString(account.AuthIndex))
		out.WriteString("\">Refresh</button>")
		if !account.HostMatched {
			out.WriteString(" <button class=\"secondary\" data-delete-auth=\"")
			out.WriteString(html.EscapeString(account.AuthID))
			out.WriteString("\" data-delete-index=\"")
			out.WriteString(html.EscapeString(account.AuthIndex))
			out.WriteString("\">Remove</button>")
		}
		out.WriteString("</td></tr>")
	}
	out.WriteString("</tbody></table><h2>Config</h2><pre>")
	out.WriteString(html.EscapeString(prettyJSON(status.Config)))
	out.WriteString("</pre>")
	out.WriteString(quotaGuardStatusScript)
	out.WriteString("</main></body></html>")
	return out.Bytes()
}

func renderHealthObservations(out *bytes.Buffer, observations []modelHealthObservation, now time.Time) {
	if len(observations) == 0 {
		return
	}
	cutoff := now.Add(-time.Hour)
	out.WriteString("<details class=\"health-details\"><summary>Health by model</summary><table><thead><tr><th>Model</th><th>API key</th><th>60m result</th><th>Overload</th><th>Inflight</th></tr></thead><tbody>")
	for _, observation := range observations {
		total := healthObservationTotals(observation, cutoff)
		if total.Requests == 0 && total.Failures == 0 && observation.CurrentInflight == 0 {
			continue
		}
		out.WriteString("<tr><td>")
		out.WriteString(html.EscapeString(firstNonEmpty(observation.Model, "unknown")))
		out.WriteString("</td><td>")
		out.WriteString(html.EscapeString(firstNonEmpty(observation.APIKeyID, "unknown")))
		out.WriteString("</td><td>")
		out.WriteString(strconv.FormatInt(total.Successes, 10))
		out.WriteString(" ok / ")
		out.WriteString(strconv.FormatInt(total.Failures, 10))
		out.WriteString(" fail")
		out.WriteString("</td><td>")
		out.WriteString(strconv.FormatInt(total.OverloadFailures, 10))
		out.WriteString("</td><td>")
		out.WriteString(strconv.Itoa(observation.CurrentInflight))
		out.WriteString(" current / ")
		out.WriteString(strconv.Itoa(observation.MaxInflight))
		out.WriteString(" max</td></tr>")
	}
	out.WriteString("</tbody></table></details>")
}

func renderAffinitySection(out *bytes.Buffer, affinity affinitySnapshot, accounts []accountSnapshot) {
	out.WriteString("<div class=\"section\"><h2>Affinity</h2>")
	out.WriteString("<p>")
	if affinity.Enabled {
		out.WriteString("<span class=\"pill good\">enabled</span>")
	} else {
		out.WriteString("<span class=\"pill\">disabled</span>")
	}
	out.WriteString(" header <code>")
	out.WriteString(html.EscapeString(firstNonEmpty(affinity.Header, "X-CPA-Client-ID")))
	out.WriteString("</code> <span class=\"muted\">missing header uses legacy/global primary</span></p>")
	out.WriteString("<p class=\"muted\">Capacity controls the long-term share, while the minimum-load floor keeps eligible groups from staying empty. New clients prefer missing floors, then use capacity-weighted affinity. Pro/repeatable accounts remain backups where configured.</p>")
	if affinity.Rebalance.LastAttemptAt.IsZero() {
		out.WriteString("<p class=\"muted\">Rebalance has not attempted a Keeper usage collection yet.</p>")
	} else {
		out.WriteString("<p class=\"muted\">Last attempt ")
		out.WriteString(html.EscapeString(affinity.Rebalance.LastAttemptAt.Format(time.RFC3339)))
		if affinity.Rebalance.LastAnalysisAt.IsZero() {
			out.WriteString(" · <span class=\"low\">no completed analysis yet</span>")
		} else {
			out.WriteString(" · last analysis ")
			out.WriteString(html.EscapeString(affinity.Rebalance.LastAnalysisAt.Format(time.RFC3339)))
			staleAfter := time.Duration(affinity.RebalanceInterval*2) * time.Second
			if staleAfter > 0 && time.Since(affinity.Rebalance.LastAnalysisAt) > staleAfter {
				out.WriteString(" · <span class=\"low\">analysis is stale</span>")
			}
		}
		if affinity.Rebalance.LastError != "" {
			out.WriteString(" · <span class=\"low\">")
			out.WriteString(html.EscapeString(affinity.Rebalance.LastError))
			out.WriteString("</span>")
		}
		out.WriteString("</p>")
	}
	out.WriteString("<div class=\"toolbar\"><button class=\"secondary\" id=\"quota-guard-rebalance-analyze\">Analyze Now</button><button class=\"secondary\" id=\"quota-guard-rebalance-once\">Rebalance Once</button></div>")
	renderManualGroupEditor(out, accounts)
	if len(affinity.Groups) > 0 {
		out.WriteString("<table><thead><tr><th>Group</th><th>Members</th><th>Current</th><th>Status</th><th>60m Load</th><th>Floor</th><th>Bindings</th><th>Action</th></tr></thead><tbody>")
		for _, group := range affinity.Groups {
			out.WriteString("<tr><td><code>")
			out.WriteString(html.EscapeString(group.ID))
			out.WriteString("</code><br><span class=\"muted\">")
			out.WriteString(html.EscapeString(firstNonEmpty(group.Source, "auto")))
			out.WriteString(" weight ")
			out.WriteString(formatScore(group.Weight))
			out.WriteString("</span></td><td>")
			for _, member := range group.Members {
				out.WriteString("<div><code>")
				out.WriteString(html.EscapeString(member))
				out.WriteString("</code>")
				switch {
				case member == group.MainAuthID:
					out.WriteString(" <span class=\"pill good\">main</span>")
				case stringSliceContains(group.BackupAuthIDs, member):
					out.WriteString(" <span class=\"pill\">backup</span>")
				}
				out.WriteString("</div>")
			}
			out.WriteString("</td><td>")
			if group.CurrentAuthID != "" {
				out.WriteString("<code>")
				out.WriteString(html.EscapeString(group.CurrentAuthID))
				out.WriteString("</code>")
				if group.CurrentAuthIndex != "" {
					out.WriteString("<br><span class=\"muted\">")
					out.WriteString(html.EscapeString(group.CurrentAuthIndex))
					out.WriteString("</span>")
				}
			} else {
				out.WriteString("<span class=\"muted\">none yet</span>")
			}
			out.WriteString("</td><td>")
			if group.Eligible {
				out.WriteString("<span class=\"pill good\">eligible</span>")
			} else {
				out.WriteString("<span class=\"pill bad\">skipped</span>")
			}
			if group.Reason != "" {
				out.WriteString("<br><span class=\"muted\">")
				out.WriteString(html.EscapeString(group.Reason))
				out.WriteString("</span>")
			}
			out.WriteString("</td><td class=\"load-cell\">")
			out.WriteString("<span title=\"")
			out.WriteString(html.EscapeString(groupLoadDetails(group, affinity.FastWindowMinutes)))
			out.WriteString("\"><strong>")
			out.WriteString(fmt.Sprintf("%.2f%%", group.ActualShare))
			out.WriteString("</strong> actual · ")
			out.WriteString(fmt.Sprintf("%.2f%%", group.TargetShare))
			out.WriteString(" target<br>")
			out.WriteString(formatScore(group.Tokens60m))
			out.WriteString(" · ")
			out.WriteString(fmt.Sprintf("%.2fx", group.LoadFactor))
			out.WriteString(" pressure<span class=\"muted\">")
			if group.FiveHourActive {
				out.WriteString("<br>5h ")
				out.WriteString(fmt.Sprintf("%.2f%%", group.FiveHourRemaining))
			}
			out.WriteString(" · W ")
			out.WriteString(fmt.Sprintf("%.2f%%", group.WeeklyRemaining))
			out.WriteString(" · T+1 ")
			out.WriteString(fmt.Sprintf("%.2fx", group.BudgetMultiplier))
			out.WriteString("</span></span>")
			out.WriteString("</td><td>")
			out.WriteString("<span class=\"pill ")
			if group.FloorState == "satisfied" {
				out.WriteString("good")
			} else if group.FloorState == "ineligible" {
				out.WriteString("bad")
			}
			out.WriteString("\">")
			out.WriteString(html.EscapeString(firstNonEmpty(group.FloorState, "unknown")))
			out.WriteString("</span><br><span class=\"muted\">source ")
			out.WriteString(html.EscapeString(firstNonEmpty(group.Attribution, "unknown")))
			out.WriteString("<br>API ")
			out.WriteString(formatScore(group.APIKeyTokens))
			out.WriteString(" · local ")
			out.WriteString(formatScore(group.LocalTokens))
			if group.UnattributedTokens > 0 {
				out.WriteString(" · unknown ")
				out.WriteString(formatScore(group.UnattributedTokens))
			}
			out.WriteString("</span></td><td>")
			out.WriteString(strconv.Itoa(group.BindingCount))
			out.WriteString(" persistent<br><span class=\"muted\">")
			out.WriteString(strconv.Itoa(group.ActiveBindingCount))
			out.WriteString(" active 24h</span>")
			out.WriteString("</td><td>")
			if group.Source == "manual-state" {
				out.WriteString("<button class=\"secondary\" data-delete-group=\"")
				out.WriteString(html.EscapeString(group.ID))
				out.WriteString("\">Delete</button>")
			} else if group.Source == "auto" {
				out.WriteString("<button class=\"secondary\" data-create-group=\"")
				out.WriteString(html.EscapeString(group.ID))
				out.WriteString("\" data-members=\"")
				out.WriteString(html.EscapeString(strings.Join(group.Members, ",")))
				out.WriteString("\">Create Manual Override</button>")
			} else {
				out.WriteString("<span class=\"muted\">")
				out.WriteString(html.EscapeString(firstNonEmpty(group.Source, "auto")))
				out.WriteString("</span>")
			}
			out.WriteString("</td></tr>")
		}
		out.WriteString("</tbody></table>")
	}
	if len(affinity.Bindings) > 0 {
		out.WriteString("<details><summary>Client Bindings (")
		out.WriteString(strconv.Itoa(len(affinity.Bindings)))
		out.WriteString(")</summary><div class=\"toolbar\"><button class=\"secondary\" id=\"quota-guard-delete-bindings\">Delete Selected</button><select id=\"quota-guard-move-target\"><option value=\"\">Move to group...</option>")
		for _, group := range affinity.Groups {
			if !group.Eligible {
				continue
			}
			out.WriteString("<option value=\"")
			out.WriteString(html.EscapeString(group.ID))
			out.WriteString("\">")
			out.WriteString(html.EscapeString(group.ID))
			out.WriteString(" (")
			out.WriteString(strconv.Itoa(group.BindingCount))
			out.WriteString(" persistent / ")
			out.WriteString(strconv.Itoa(group.ActiveBindingCount))
			out.WriteString(" active")
			out.WriteString(")</option>")
		}
		out.WriteString("</select><button class=\"secondary\" id=\"quota-guard-move-bindings\">Move Selected</button></div><table><thead><tr><th><input type=\"checkbox\" id=\"quota-guard-bindings-all\"></th><th>Client</th><th>Group</th><th>60m Activity</th><th>Last Seen</th><th>Move State</th></tr></thead><tbody>")
		for _, binding := range affinity.Bindings {
			out.WriteString("<tr><td><input type=\"checkbox\" name=\"quota-guard-client-binding\" value=\"")
			out.WriteString(html.EscapeString(binding.ClientID))
			out.WriteString("\"></td><td><code>")
			out.WriteString(html.EscapeString(binding.ClientID))
			out.WriteString("</code></td><td><code>")
			out.WriteString(html.EscapeString(binding.GroupID))
			out.WriteString("</code></td><td class=\"nowrap\">")
			if binding.APIKeyID != "" {
				out.WriteString("<span class=\"muted\">API key ")
				out.WriteString(html.EscapeString(binding.APIKeyID))
				out.WriteString("</span><br>")
			}
			out.WriteString(strconv.Itoa(binding.Picks60m))
			out.WriteString(" picks<br><span class=\"muted\">")
			out.WriteString(formatScore(binding.UsageScore60m))
			out.WriteString(" local score")
			if binding.APIKeyTokens60m > 0 {
				out.WriteString(" · API ")
				out.WriteString(formatScore(binding.APIKeyTokens60m))
			}
			out.WriteString("<br>")
			out.WriteString(html.EscapeString(firstNonEmpty(binding.UsageSource, "unknown")))
			if !binding.LastHeaderActivity.IsZero() {
				out.WriteString("<br>header last ")
				out.WriteString(html.EscapeString(binding.LastHeaderActivity.Format("01-02 15:04")))
			}
			out.WriteString("</span></td><td>")
			if !binding.LastSeenAt.IsZero() {
				out.WriteString(html.EscapeString(binding.LastSeenAt.Format(time.RFC3339)))
			}
			out.WriteString("</td><td class=\"tight\">")
			if !binding.CooldownUntil.IsZero() && binding.CooldownUntil.After(time.Now()) {
				out.WriteString("<span class=\"pill\">cooldown</span><br><span class=\"muted\">")
				out.WriteString(html.EscapeString(binding.CooldownUntil.Format("01-02 15:04")))
				out.WriteString("</span>")
			} else {
				out.WriteString("<span class=\"pill good\">movable when idle</span>")
			}
			if binding.LastMoveReason != "" {
				out.WriteString("<br><span class=\"muted\">")
				out.WriteString(html.EscapeString(binding.LastMoveReason))
				out.WriteString("</span>")
			}
			out.WriteString("</td></tr>")
		}
		out.WriteString("</tbody></table></details>")
	}
	if len(affinity.Rebalance.History) > 0 {
		moves := make([]rebalanceHistoryEntry, 0)
		analysis := make([]rebalanceHistoryEntry, 0)
		for _, entry := range affinity.Rebalance.History {
			if entry.Action == "analyze" {
				analysis = append(analysis, entry)
			} else {
				moves = append(moves, entry)
			}
		}
		if len(moves) > 0 {
			out.WriteString("<details><summary>Rebalance Moves (" + strconv.Itoa(len(moves)) + ")</summary><p class=\"muted\">Only actual binding changes and errors are shown here.</p><table><thead><tr><th>Time</th><th>Result</th><th>Client</th><th>Route</th><th>Reason</th></tr></thead><tbody>")
			start := len(moves) - 20
			if start < 0 {
				start = 0
			}
			for i := len(moves) - 1; i >= start; i-- {
				entry := moves[i]
				out.WriteString("<tr><td class=\"nowrap\">" + html.EscapeString(entry.At.Format("01-02 15:04")) + "</td><td>" + html.EscapeString(entry.Action+" / "+entry.Result) + "</td><td><code>" + html.EscapeString(entry.ClientID) + "</code></td><td><code>" + html.EscapeString(entry.FromGroup) + "</code> → <code>" + html.EscapeString(entry.ToGroup) + "</code></td><td>" + html.EscapeString(entry.Reason) + "</td></tr>")
			}
			if len(moves) > 20 {
				out.WriteString("<tr><td colspan=\"5\" class=\"muted\">Showing the latest 20 of ")
				out.WriteString(strconv.Itoa(len(moves)))
				out.WriteString(" actual events.</td></tr>")
			}
			out.WriteString("</tbody></table></details>")
		}
		if len(analysis) > 0 {
			out.WriteString("<details><summary>Analysis Log (" + strconv.Itoa(len(analysis)) + ")</summary><p class=\"muted\">Analysis-only records do not change client bindings. Showing the latest 20.</p><table><thead><tr><th>Time</th><th>Result</th><th>Client</th><th>Route</th><th>Reason</th></tr></thead><tbody>")
			start := len(analysis) - 20
			if start < 0 {
				start = 0
			}
			for i := len(analysis) - 1; i >= start; i-- {
				entry := analysis[i]
				out.WriteString("<tr><td class=\"nowrap\">" + html.EscapeString(entry.At.Format("01-02 15:04")) + "</td><td>" + html.EscapeString(entry.Result) + "</td><td><code>" + html.EscapeString(entry.ClientID) + "</code></td><td><code>" + html.EscapeString(entry.FromGroup) + "</code> → <code>" + html.EscapeString(entry.ToGroup) + "</code></td><td>" + html.EscapeString(entry.Reason) + "</td></tr>")
			}
			out.WriteString("</tbody></table></details>")
		}
	}
	out.WriteString("</div>")
}

func renderManualGroupEditor(out *bytes.Buffer, accounts []accountSnapshot) {
	editable := manualCalibrateAccounts(accounts)
	if len(editable) == 0 {
		return
	}
	out.WriteString("<details id=\"quota-guard-manual-group-details\"><summary>Manual Groups</summary><form class=\"manual\" id=\"quota-guard-manual-group\"><input name=\"group_id\" placeholder=\"group id\" required>")
	out.WriteString("<div>")
	for _, account := range editable {
		out.WriteString("<label class=\"pill\"><input type=\"checkbox\" name=\"member\" value=\"")
		out.WriteString(html.EscapeString(account.AuthID))
		out.WriteString("\"> ")
		out.WriteString(html.EscapeString(manualCalibrateAccountLabel(account)))
		out.WriteString("</label> ")
	}
	out.WriteString("</div><button>Save Group</button></form></details>")
}

func renderQuotaLines(out *bytes.Buffer, account accountSnapshot) {
	out.WriteString("<div class=\"quota-lines\">")
	for _, window := range account.ActiveWindows {
		remaining := account.WindowRemaining[window]
		out.WriteString("<div class=\"quota-line\"><span class=\"quota-label\">")
		out.WriteString(html.EscapeString(quotaWindowLabel(window)))
		out.WriteString("</span><strong>")
		out.WriteString(fmt.Sprintf("%.2f%%", remaining))
		if snap, ok := account.QuotaSnapshots[window]; ok {
			if snap.ResetAt != nil {
				out.WriteString(" <span class=\"muted\">· ")
				out.WriteString(html.EscapeString(snap.ResetAt.In(weeklyBudgetLocation(8)).Format("01-02 15:04")))
				out.WriteString("</span>")
			}
		}
		out.WriteString("</strong></div>")
	}
	out.WriteString("</div>")
}

func groupLoadDetails(group affinityGroupSnapshot, fastWindowMinutes int64) string {
	parts := []string{
		fmt.Sprintf("%dm %s", fastWindowMinutes, formatScore(group.FastTokens)),
		"60m " + formatScore(group.SlowTokens),
		"capacity " + formatScore(group.MainCapacity),
		"streak " + strconv.Itoa(group.OverloadStreak),
	}
	if group.ExpectedRemaining > 0 {
		parts = append(parts, fmt.Sprintf("trajectory %.2f%%", group.ExpectedRemaining))
	}
	if group.DailyBurn > 0 || group.ExpectedDailyBurn > 0 {
		parts = append(parts, fmt.Sprintf("daily burn %.2f%% / %.2f%% expected", group.DailyBurn, group.ExpectedDailyBurn))
	}
	if group.BudgetReason != "" {
		parts = append(parts, group.BudgetReason)
	}
	return strings.Join(parts, " | ")
}

func manualCalibrateAccountLabel(account accountSnapshot) string {
	parts := []string{account.AuthID}
	if account.AuthIndex != "" {
		parts = append(parts, account.AuthIndex)
	}
	if account.Provider != "" {
		parts = append(parts, account.Provider)
	}
	return strings.Join(parts, " · ")
}

func manualCalibrateAccounts(accounts []accountSnapshot) []accountSnapshot {
	byIndex := map[string]bool{}
	for _, account := range accounts {
		if account.AuthIndex != "" && account.AuthID != account.AuthIndex {
			byIndex[account.AuthIndex] = true
		}
	}
	out := make([]accountSnapshot, 0, len(accounts))
	for _, account := range accounts {
		if account.AuthID == "" {
			continue
		}
		if account.AuthID == account.AuthIndex && byIndex[account.AuthIndex] {
			continue
		}
		if account.AuthIndex == "" && byIndex[account.AuthID] {
			continue
		}
		out = append(out, account)
	}
	return out
}

func recentSummary(account accountSnapshot) string {
	summary := fmt.Sprintf("ok %d / fail %d", account.Success, account.Failed)
	for i := len(account.RecentRequests) - 1; i >= 0; i-- {
		bucket := account.RecentRequests[i]
		if bucket.Success == 0 && bucket.Failed == 0 {
			continue
		}
		return fmt.Sprintf("%s\nlast %s: ok %d / fail %d", summary, bucket.Time, bucket.Success, bucket.Failed)
	}
	return summary
}
