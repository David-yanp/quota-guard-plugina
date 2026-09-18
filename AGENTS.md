# AGENTS.md

## Source Layout

- `plugin_entry.go` contains the c-shared ABI entry points and host callback bridge.
- `models.go` contains plugin configuration, persisted state, and API snapshot models.
- `config.go` contains lifecycle, configuration normalization, and background refresh startup.
- `scheduler.go` contains legacy fill-first, affinity group selection, and temporary failover.
- `usage.go` contains usage scoring, inflight reservations, and local account usage state.
- `quota.go` contains Codex/Keeper quota parsing and window calculations.
- `status.go` contains account eligibility and status snapshots.
- `management.go` and `admin.go` contain management/resource routes and quota actions.
- `rebalance.go` contains Keeper load analysis and client binding moves.
- `ui.go` and `ui_assets.go` contain the resource page renderer and browser assets.
- `state.go` contains state persistence and calibration/reset helpers.

Keep new behavior in the narrowest matching file. Do not return to a monolithic `main.go`.

Quota Guard is a CLIProxyAPI plugin that adds quota-aware fill-first scheduling without changing upstream CLIProxyAPI source code.

## Scope
- This is the standalone quota-guard plugin project. Keep plugin behavior inside this repository.
- The built runtime artifact is `dist/quota-guard.so` before installation, and usually `plugins/quota-guard.so` after copying into a CLIProxyAPI deployment.
- Do not modify official CLIProxyAPI core scheduler, usage, management, translator, or runtime source to implement quota-guard behavior.
- Prefer configuration and plugin protocol capabilities over source patches so future upstream sync remains easy.

## Goals
- Preserve fill-first account order, but switch before the active account is exhausted.
- Use `min_remaining_percent` as the safety floor. Current production target is `10`.
- Apply to every scheduler candidate supplied by the host, not only Codex candidates.
- Exclude disabled, unavailable, and status-disabled candidates from scheduling.
- Only candidates with `status=active` are normally eligible for scheduling. Empty status values are rejected and shown as diagnostics.
- Exception: with `request_error_status_override_enabled: true`, a host `status=error` account may remain eligible only when `status_message` clearly indicates a request-scoped/client lifecycle failure (`context_too_large`, `invalid_request_error`, `context canceled` / `context cancelled`), the account is not disabled/unavailable, and `next_retry_after` is not in the future.
- Do not use this exception for credential/provider failures. Messages containing 401, 429, Cloudflare/challenge, forbidden, unauthorized, quota, rate limit, or payment_required must remain ineligible.
- Continue tracking empty scheduler candidate status as a diagnostic signal.
- Treat first-seen accounts as usable with default limits so fill-first startup works naturally.
- Record the selected account as `current_role=primary` after `scheduler.pick`.

## Quota Model
- Codex accounts should prefer real quota snapshots from host auth JSON / CPA Usage Keeper.
- Parse official primary 5h and Weekly buckets. Plus/Team accounts use 5h as the short-term scheduling gate and headline quota, while Weekly remains the long-term capacity and T+1 budget.
- Pro accounts normally expose only a primary Weekly bucket. Ignore model-specific `additional_rate_limits.*` buckets such as GPT-5.3-Codex-Spark 5h/Weekly (`codex_bengalfox`); they must never become general account windows. Monthly buckets remain inactive.
- Keeper refresh is configured through:
  - `quota_refresh_trigger_endpoint`, default `http://cpa-usage-keeper:8080/cpa/api/v1/quota/refresh`
  - `quota_refresh_endpoint`, default `http://cpa-usage-keeper:8080/cpa/api/v1/quota/refresh/{auth_index}`
- Background refresh from `quota_refresh_interval_seconds` should always refresh the current selected `primary` account.
- Background refresh must also refresh any Codex account whose Weekly reset time has passed, even when it is not the current primary.
- Reset-expired background refreshes should be forced once after the reset time. Do not repeat the same reset refresh after `last_quota_refresh_at` is newer than that window reset time.
- Manual UI/API refresh may still target one account or all accounts for diagnostics.
- Real Keeper/auth quota snapshots are authoritative percentages for scheduling.
- Completed local usage after a real snapshot is diagnostic only because local token score does not map reliably to official quota percentages.
- Inflight reservations are temporary scheduling deductions only. They are not actual usage.
- Current `inflight_reserve_score` is `30000`, derived from CPA Usage Keeper request distribution:
  - recent Codex success P90 is about 231k score
  - observed same-auth concurrency is usually 6-7
  - `30000` is roughly P90 divided by observed concurrency, rounded down to reduce short switch-back oscillation
- `sticky_current_auth_seconds` is retained only for config compatibility. Core scheduling must use reserve-until-low primary stickiness instead of a time window.

## Primary Stickiness
- Current primary means the last account selected by `scheduler.pick`.
- If current primary is present in the host candidate list and remains eligible, keep using it even if another same-priority account sorts earlier by auth ID.
- Only reselect when current primary is missing from candidates, disabled, unavailable, not `status=active`, or any active primary 5h/Weekly window is below `min_remaining_percent`.
- Fill-first ordering (`priority desc`, then stable auth ID order) is used only for initial selection and reselection after current primary becomes ineligible.
- Each selected request should still add an inflight reservation and refresh `current_auth_id`, `current_auth_index`, `current_role=primary`, and `last_selected_at`.
- The status page must not show an ineligible account as active `primary`. If `current_auth_id` is no longer eligible, show it as `stale primary` / `last selected` and make clear the next scheduler pick will reselect.

## Window Semantics
- Scheduling eligibility and reserve checks use every active primary window: 5h and Weekly must both remain above the reserve. If an account has no primary 5h window, Weekly alone is authoritative.
- Account headline remaining prefers primary 5h when present, while the UI must also show Weekly and both reset times.
- Target Share, effective capacity, T+1, and long-term rebalance normalization use Weekly only. Do not use the fast 5h cycle as a persistent Client Binding weight.
- Use the Weekly snapshot's reported `limit_score`; Plus/Team and Pro accounts may have materially different Weekly capacities.
- Effective capacity is `(weekly_remaining - reserve) × weekly_limit_score`, multiplied by the bounded T+1 daily budget adjustment.
- Local token score does not estimate official Weekly consumption. It is used only for request attribution and choosing which client binding is safe to move.
- Each fresh official Weekly snapshot updates a UTC+8 daily sample. On the next local day, compare actual burn and trajectory remaining with the expected burn, then adjust capacity by at most `weekly_budget_max_adjustment_percent`.
- Detect a changed Weekly `reset_at` as a new cycle and establish a fresh baseline.
- T+1 samples are bucketed at UTC+8 `00:00`. Use the first fresh official snapshot after midnight as the observation for that day, and require at least 20 hours between observations before applying an adjustment.
- Do not reset a Weekly cycle merely because `reset_at` changes. Preserve samples for reset drift alone. Before the old boundary, accept an early real reset only when the new reset advances by more than one day and quota either returns to at least 95% or rises by at least 20 percentage points.

## Resource UI
- Primary page: `/v0/resource/plugins/quota-guard/status`.
- Keep the page compact and operational:
  - show current `primary`
  - show auth ID/index, provider, priority, host status, eligibility reason
  - show primary 5h and Weekly as separate, clearly labelled remaining percentages and reset times
  - show active windows, reset time, source, refresh time, usage since refresh, and inflight count/score
  - avoid wide recent-request columns that stretch the page
- Resource page actions should not require the management key when `resource_actions_require_management_key: false`; protect the route externally with IP/reverse-proxy restrictions.
- `Refresh` must refresh quota snapshots, not reset account status.
- Do not expose `Manual Calibrate` in the resource UI or management API; real quota snapshots and refresh actions are the supported path.
- `Client Bindings` should be collapsed by default and kept permanently for affinity stability.
- `Client Bindings` must be sorted by `Last Seen` descending, with a stable client ID fallback for equal timestamps.
- Do not auto-prune `X-CPA-Client-ID` bindings by age. Stale or mistaken bindings must be removed manually from the resource UI checkbox delete action.
- Keep the checkbox `Delete Selected` action for obsolete or wrong bindings.
- Keep the `Move Selected` action for manual rebalance. It should move selected bindings to an existing eligible group only, update `GroupID`, `UpdatedAt`, and `LastSeenAt`, and must not delete group/account/quota/current state.
- Automatic affinity groups should use Plus/Team style accounts as the main member and Pro / explicitly repeatable accounts only as backup members. A Pro backup must not take primary group traffic while the main account remains eligible above reserve.
- Automatic group IDs should be stable for the main account so existing client bindings do not drift when candidate order changes.
- Stable group IDs are not enough to balance usage. New-client assignment must first satisfy the minimum binding/traffic floor, then use capacity-weighted rendezvous; existing bindings stay sticky unless the bound group is unavailable or an operator deletes/moves them.
- `legacy_primary_auth` is evaluated before the remembered global primary for requests without the affinity header. It must fall back to normal fill-first when the configured auth is not eligible.
- A configured one-member affinity group is valid for an explicitly reserved account and excludes that account from automatic backup placement in other groups.
- Optional load rebalancing must use Keeper realtime auth-file usage as the group-level source of truth and plugin-side scheduler/usage activity only to estimate each client's share.
- Normalize group load by the main account's Weekly usable capacity and T+1 budget multiplier. Show primary 5h as a short-term availability signal, but do not include it in Target Share. Do not count a shared Pro/repeatable backup as capacity in every group.
- Predict load from Keeper's minimum supported 15-minute rate and the slow 60-minute rate, weighted 70/30 by default. Normalize unsupported fast windows to 15 minutes.
- Scale main-account capacity by quota remaining above the reserve floor so depleted groups receive less target traffic.
- Automatic rebalance candidates must have activity inside the slow window and be outside automatic/manual move cooldowns. Bindings with no activity stay unchanged.
- Treat recent activity and client inflight count as bounded move penalties, not hard blockers. Existing inflight requests finish on the old group; only subsequent picks use the new binding.
- Require three consecutive overload samples by default, with source pressure >= 1.25 and target pressure <= 0.85.
- Evaluate all safe client/target pairs and choose the one move that most reduces maximum normalized group pressure. Rebalance at most one binding per default 5-minute cycle.
- When the target group has zero slow-window traffic, use a guarded 5% minimum improvement floor instead of the normal 15% floor so empty eligible groups can warm up gradually without moving active clients aggressively.
- Automatic moves cool down for 6 hours in the formal configuration; manual moves cool down for 24 hours.
- Eligible groups use a minimum-load floor: persistent bindings preserve affinity, but only Header-observed activity in `client_affinity_dormant_seconds` counts toward the active floor. New clients prefer groups missing active Header traffic and then groups below the traffic floor before capacity-weighted rendezvous.
- The active binding floor is a hard constraint for automatic rebalance, not only a new-client preference. An eligible source group must retain at least `client_affinity_min_binding_per_group` active Header client; a dormant historical binding never blocks another real client from being assigned to that group.
- When an eligible group is below the active binding floor, rebalance may prioritize a quiet client from a source group with more than the active floor even when the normal global pressure improvement threshold is not met. If regular groups are missing their floor, standalone Pro/repeatable groups are not ordinary rebalance targets.
- Keeper API-key usage must be attributed to a group only when a unique current-window plugin Header route proves the API key, group, and auth relationship. API-key-only or ambiguous shared-key usage stays diagnostic/unattributed and must not be copied, split, or deducted from auth-file traffic for balancing.
- Scheduler metadata `api_key_id` is recorded with the client binding and inflight/usage activity. Keeper realtime API-key usage is preferred for client load; local plugin activity is the fallback when Keeper data is missing.
- When the host does not provide `api_key_id`, use the configured `client_affinity_api_key_map` (`client_id -> Keeper numeric API key ID`) before falling back to local activity. Never store full API-key secrets.
- Shared repeatable-account usage is not copied into every group. Known API-key or recorded group activity is attributed to the owning group; residual usage is marked partial/unattributed and cannot trigger aggressive moves.
- In `auto` mode, assign new clients with capacity-weighted rendezvous hashing. In `observe`, preserve existing assignment semantics and never mutate bindings.
- Shared backup usage must be allocated by API-key data or recorded group picks. If attribution is unavailable, mark the affected load partial and avoid aggressive moves without blocking normal scheduling.
- Keeper `auth_files: []` is a valid zero-traffic snapshot, not an endpoint failure. Record a skipped analysis with `no usage in analysis window`, clear prior Keeper errors, and do not move bindings.
- `observe` mode is required for grey rollout. It may analyze and record recommendations but must not mutate bindings.
- Persist rebalance history with the source/target groups, client, load metrics, idle duration, predicted improvement, result, and reason.
- Keep analysis-only history separate from actual binding changes in the resource UI. Show only the latest 20 rows in each section; retain the full bounded history in state for diagnosis.
- If current primary becomes skipped after refresh or status changes, show stale/last-selected state only. Do not mutate scheduler state from UI refresh; actual switching belongs to the next `scheduler.pick`.
- Quota snapshots older than `quota_snapshot_max_age_seconds` are not fresh authority. Until refresh succeeds, the old snapshot may be used only as a conservative lower bound, with local usage since `snap.At` and inflight reservations deducted.
- A bound affinity group becoming temporarily unavailable must use a request-scoped failover. Do not permanently change `ClientBindings[client_id].GroupID` from the scheduler failover path; record failover metadata separately.
- Match the visual theme of official `management.html`. Use the same browser theme key (`cli-proxy-theme`) and compatible CSS variables such as `--bg-primary`, `--bg-secondary`, `--text-primary`, `--border-color`, and `--primary-color`.
- Support `white`, `dark`, and `auto` theme behavior. Avoid hard-coded slate/teal color palettes that diverge from the management panel.

## Management API
- Keep management routes under `/v0/management/plugins/quota-guard/`.
- Keep resource routes under `/v0/resource/plugins/quota-guard/`.
- Management APIs remain key-protected by host policy.
- Resource UI may use unkeyed actions only when explicitly configured.

## State
- Persist plugin state in `plugins/quota-guard-state.json` unless overridden.
- State may include auth ID/index, quota snapshots, usage events, inflight reservations, refresh timestamps, current primary, client bindings, manual groups, and request counters.
- Provide UI/API cleanup for mistaken local state entries instead of requiring manual JSON edits.

## Build And Verify
```bash
GOCACHE=/tmp/go-build-cache GOMODCACHE=/tmp/go-mod-cache /usr/local/go/bin/go test ./...
mkdir -p dist
GOCACHE=/tmp/go-build-cache GOMODCACHE=/tmp/go-mod-cache /usr/local/go/bin/go build -buildmode=c-shared -o dist/quota-guard.so .
```

After Go changes, format with:

```bash
/usr/local/go/bin/gofmt -w main.go main_test.go
```

For local development against an unreleased CLIProxyAPI checkout, temporarily add:

```bash
go mod edit -replace github.com/router-for-me/CLIProxyAPI/v7=/path/to/CLIProxyAPI
```

Remove the local replace before release:

```bash
go mod edit -dropreplace github.com/router-for-me/CLIProxyAPI/v7
go mod tidy
```

## Runtime Notes
- Updating `plugins/quota-guard.so` requires restarting the CLIProxyAPI process/container because Go plugins are loaded into the running process.
- Config changes should also be applied with a service restart unless host hot-reload behavior has been verified for plugin configs.
- Quota refresh and rebalance run in independent background loops. A stalled quota refresh must never prevent the rebalance ticker from attempting its scheduled Keeper collection.
- Rebalance health is tracked as `last_attempt_at` and `last_analysis_at`. The resource page flags analysis as stale after two configured rebalance intervals.
- Quota refresh tracks successful snapshot time separately from refresh attempts. A failed refresh must not advance `last_quota_refresh_at`; when the retained snapshot is stale, the UI must state `snapshot expired; refresh failed` and show the failure reason.
- A Pro account may be both a standalone manual-group main and a repeatable automatic backup. Add its auth ID or auth index to `client_affinity_groups` and `client_affinity_repeatable_auths` to enable this role.
- Auth indexes can change when an auth file is replaced or re-created. Update `legacy_primary_auth`, `client_affinity_groups`, and `client_affinity_repeatable_auths` together; migrate bindings from the obsolete standalone group to the replacement group to preserve affinity.
- Long T+1 diagnostic reasons must render in a wrapping, width-bounded container. Do not put the group load cell under the global `.nowrap` style.
- Keep the status page compact: the group load column shows only Actual/Target, predicted tokens, pressure, quota percentages, and T+1 multiplier. Put verbose trajectory/burn/reason diagnostics in a tooltip. The Client Bindings, Rebalance Moves, and Analysis Log `<details>` must remain collapsed by default.
- The account Quota column should show only labelled remaining percentages and reset times, plus the compact quota mode; omit duplicate local-base and since-refresh details from the main table.
- Per-auth proxy restoration is plugin-only: match a Codex auth by stable `account_id` first and normalized email second, read the latest JSON with `host.auth.get`, add only a missing `proxy_url`, and persist with `host.auth.save`. Never overwrite a non-empty proxy automatically.
- Store proxy mappings in a separate `0600` JSON file, not the displayed plugin YAML config. Logs and UI must redact the host and credentials while preserving the scheme and port as `socks5://*:5556`.
- Wait for the OAuth file to settle before saving, preserve the complete latest token JSON, record restore status/errors, and keep the reconciler independent from quota refresh and rebalance loops.
- A binding whose group was removed from the rebuilt topology is migrated and persisted only when that client next makes a request. Record `topology rebind: <old> removed -> <new>` as the reason. A group that still exists but is temporarily ineligible must use request-scoped failover without changing the persistent binding.
- Current formal service is normally exposed on `127.0.0.1:8317`; grey verification has used `127.0.0.1:18317`.
- Do not remove or overwrite user backups under `backups/`.

## Todo List
- Implemented in plugin: identity affinity with `X-CPA-Client-ID`.
  - Why: CPA official source currently does not expose a stable API-key identity to scheduler plugins, and modifying official source would make future upstream sync harder. A client-supplied stable header gives quota-guard an API-key-like identity without changing CPA core code.
  - Behavior: read `X-CPA-Client-ID` from `SchedulerPickRequest.Options.Headers`; bind that identity to an affinity group; select a group primary using quota-guard's existing eligibility and reserve rules; keep the group binding while the group has an eligible member.
  - Required precedence: prefer `X-CPA-Client-ID` over session-style headers because it represents a client/API-key identity, not just one conversation.
  - Required safety: never store or display real API keys; the header value must be a non-secret stable client/team ID such as `client-a` or `team-01`.
  - Required fallback: if `X-CPA-Client-ID` is absent, keep current reserve-until-low fill-first behavior, and later optionally use `X-Session-ID`, `Session-Id` / `Session_id`, or `Options.Metadata.user_id` for session affinity.
  - Required scope: keep the implementation fully inside the quota-guard plugin; do not patch CPA official scheduler/auth source unless the plugin protocol later needs an upstream-compatible extension.
  - Grey rollout: enable and verify first through `docker-compose.quota-guard-verify.yml`, `config.quota-guard-verify.yaml`, and `plugins/quota-guard-state-verify.json` on `127.0.0.1:18317`; do not restart formal `cli-proxy-api` during grey validation.
