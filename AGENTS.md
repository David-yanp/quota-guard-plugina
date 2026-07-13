# AGENTS.md

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
- Keeper `rate_limit.*` / `scope=window` primary windows are authoritative for scheduling.
- Keeper `additional_rate_limits.*` model-specific windows must not override primary 5h/weekly account quota; use them only as fallback or diagnostics.
- Keeper refresh is configured through:
  - `quota_refresh_trigger_endpoint`, default `http://cpa-usage-keeper:8080/cpa/api/v1/quota/refresh`
  - `quota_refresh_endpoint`, default `http://cpa-usage-keeper:8080/cpa/api/v1/quota/refresh/{auth_index}`
- Background refresh from `quota_refresh_interval_seconds` should always refresh the current selected `primary` account.
- Background refresh must also refresh any Codex account whose quota window reset time has passed, even when it is not the current primary. This prevents an account from staying skipped after its 5h/weekly/monthly reset simply because traffic has already moved away from it.
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
- Only reselect when current primary is missing from candidates, disabled, unavailable, not `status=active`, below `min_remaining_percent`, or rejected by weekly capacity guard.
- Fill-first ordering (`priority desc`, then stable auth ID order) is used only for initial selection and reselection after current primary becomes ineligible.
- Each selected request should still add an inflight reservation and refresh `current_auth_id`, `current_auth_index`, `current_role=primary`, and `last_selected_at`.
- The status page must not show an ineligible account as active `primary`. If `current_auth_id` is no longer eligible, show it as `stale primary` / `last selected` and make clear the next scheduler pick will reselect.

## Window Semantics
- Scheduling should prefer 5h remaining when a 5h window exists.
- When both 5h and weekly windows exist, weekly must have enough absolute remaining score to cover using 5h down to the reserve floor.
- A low weekly percentage should not override 5h by itself, but insufficient weekly absolute capacity should make the account ineligible.
- Monthly-only accounts should schedule by monthly remaining.
- Weekly/7d quota should remain visible for diagnostics and capacity protection.
- Monthly quota must be detected when Keeper/auth quota marks an account as monthly/team style.
- Pro accounts require `pro_limit_multiplier` handling for local usage deltas after real snapshots.

## Resource UI
- Primary page: `/v0/resource/plugins/quota-guard/status`.
- Keep the page compact and operational:
  - show current `primary`
  - show auth ID/index, provider, priority, host status, eligibility reason
  - show quota as clear remaining percentages
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
- Stable group IDs are not client hash assignment. Current new-client assignment is `binding_count / group_weight` among eligible groups; existing bindings stay sticky unless the bound group is unavailable or an operator deletes/moves them.
- Optional load rebalancing must use Keeper realtime auth-file usage as the group-level source of truth and plugin-side scheduler/usage activity only to estimate each client's share.
- Normalize group load by the main account capacity. Do not count a shared Pro/repeatable backup as capacity in every group.
- Automatic rebalance candidates must have activity inside the configured 60-minute window, be idle for at least 10 minutes, and be outside automatic/manual move cooldowns. Bindings with no activity in the window stay unchanged.
- Rebalance at most one binding per default 10-minute cycle. Automatic moves cool down for 60 minutes; manual moves cool down for 24 hours.
- Shared backup usage must be allocated by recorded group picks. If attribution is unavailable, fail closed and record the reason instead of moving a binding.
- `observe` mode is required for grey rollout. It may analyze and record recommendations but must not mutate bindings.
- Persist rebalance history with the source/target groups, client, load metrics, idle duration, predicted improvement, result, and reason.
- If current primary becomes skipped after refresh or status changes, show stale/last-selected state only. Do not mutate scheduler state from UI refresh; actual switching belongs to the next `scheduler.pick`.
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
