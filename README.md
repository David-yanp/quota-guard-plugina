# Quota Guard Plugin

Quota Guard is a standalone scheduler, usage, and management plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It implements quota-aware `fill-first` scheduling with a configurable safety reserve: accounts are considered in priority order, but the plugin moves away from the current account before it reaches the configured quota floor.

This repository is published independently from CLIProxyAPI so the plugin can be updated or released without carrying local patches in the upstream server source tree.

## Source Layout

The Go implementation is split by responsibility: ABI/host bridge, configuration, models, scheduler, usage, quota parsing, status, management, rebalance, state persistence, and UI each live in separate files. `main.go` is intentionally only the package entry placeholder.

## Behavior

- Applies to every scheduler candidate supplied by the host. It does not filter by provider.
- Sorts candidates by highest priority, then stable auth ID order.
- Ignores disabled, unavailable, or status-disabled candidates.
- Creates default state for new accounts and treats them as 100% remaining.
- Uses official primary quota windows. Plus/Team accounts use 5h as the short-term scheduling gate and Weekly as the long-term capacity budget; Pro accounts normally use their primary Weekly window.
- Ignores model-specific `additional_rate_limits.*` buckets such as GPT-5.3-Codex-Spark 5h/Weekly, so model quota cannot incorrectly disable or inflate the whole account.
- Deducts usage and inflight reservations after the latest real quota snapshot so scheduling can react between refreshes.
- Records the currently selected account as `primary` after `scheduler.pick`.
- Can refresh Codex quota snapshots through a CPA/keeper endpoint template such as `/cpa/api/v1/quota/refresh/{auth_index}`.
- Can restore a missing Codex auth `proxy_url` after OAuth re-login from a private stable `account_id`/email mapping file, using the official `host.auth.save` callback without modifying CPA source.
- Treats narrow request-scoped failures (`context_too_large`, `invalid_request_error`, `context canceled`) as non-credential failures when the host still passes the auth as a candidate, no unavailable flag is set, and no future cooldown exists. Credential/provider failures such as 401, 429, Cloudflare challenge, unauthorized, quota, and rate-limit messages remain ineligible.
- Selects the first candidate whose active primary 5h and Weekly windows are both at or above `min_remaining_percent`. When no primary 5h exists, Weekly alone controls eligibility.
- Returns a retryable scheduler error when all eligible candidates are below the threshold if `fail_when_all_low` is true.
- Otherwise delegates to `delegate_when_unconfigured`, usually `fill-first`.

## X-CPA-Client-ID Affinity

When `client_affinity_enabled` is true, requests with `X-CPA-Client-ID` are bound to an affinity group. The binding is persistent so the same client normally keeps using the same group until that group has no eligible member.

- Missing `X-CPA-Client-ID` keeps the legacy/global reserve-until-low fill-first behavior.
- Automatic groups use Plus/Team accounts as the main member and Pro/repeatable accounts as backups.
- Group IDs are stable for the main account, for example `auto-<auth_index>`.
- New clients first satisfy the minimum binding/traffic floor, then use capacity-weighted rendezvous assignment. Existing bindings stay where they are unless the group becomes unavailable or an operator moves/deletes them.
- Pro/repeatable backup accounts may appear in multiple groups, but should not receive group primary traffic while the main member is eligible above reserve.

### Smooth Load Rebalancing

Quota Guard can combine Keeper's rolling auth-file usage with plugin-side client activity to rebalance persistent bindings without disrupting active sessions.

- Keeper `current_usage.auth_files` is authoritative for group-level tokens and requests.
- Client pick and usage activity is retained only for the configured rolling windows and cooldown periods.
- Loads are normalized by the main account's reported Weekly `limit_score` and remaining percentage above reserve. The 5h window is an eligibility gate, not a Target Share weight, so a short reset cycle cannot cause persistent Client Binding churn. This preserves the capacity difference between Plus/Team and Pro accounts; shared Pro backups are not counted as group capacity multiple times.
- A UTC+8 daily T+1 controller compares official Weekly burn with the expected Weekly trajectory. It adjusts the next day's effective capacity by at most 20% while Keeper tokens remain a client-load signal only.
- Daily samples use UTC+8 midnight boundaries. Reset-time drift is retained as diagnostics and cannot clear samples before the stable Weekly boundary; partial intervals under 20 hours establish a baseline without changing capacity.
- Predicted load combines Keeper's minimum supported 15-minute burst rate and a 60-minute baseline rate, weighted 70/30 by default.
- Effective group capacity decreases as real quota approaches the configured reserve floor.
- Active requests are never canceled. Recent activity and inflight count add a bounded migration penalty instead of permanently blocking a continuously active client; new requests use the new group while old inflight requests finish on the old group.
- Bindings with no activity in the slow window stay unchanged, and moved clients remain protected by cooldowns.
- Automatic moves are limited to one per cycle, followed by a 6-hour per-client cooldown. Manual moves receive a 24-hour cooldown.
- Overload must persist for three analysis samples. The default hysteresis requires source pressure at or above 1.25 and target pressure at or below 0.85.
- The planner evaluates every safe client/target pair and chooses the single move with the largest reduction in maximum normalized group pressure.
- An eligible target with zero slow-window traffic uses a guarded 5% improvement floor, allowing empty groups to warm up gradually while preserving idle, cooldown, and one-move-per-cycle protections.
- New clients use capacity-weighted rendezvous assignment in `auto` mode; `observe` mode preserves the existing assignment behavior.
- `legacy_primary_auth` can pin requests without `X-CPA-Client-ID` to a preferred auth ID or auth index. If it is unavailable, normal legacy fill-first fallback is used.
- A one-member `client_affinity_groups` entry can reserve a high-capacity account as a standalone group so affinity clients can be assigned to it without making it a backup in every other group.
- If an auth file replacement changes its auth index, update every configured identity reference together and migrate bindings from the obsolete standalone group. Otherwise the account may remain only as an automatic backup while its intended standalone group disappears.
- `observe` mode records recommendations without changing bindings. `auto` mode applies moves that satisfy the load-ratio and predicted-improvement guards.
- Eligible groups have a configurable minimum-load floor. Persistent bindings preserve long-term affinity, while Header-observed activity determines whether a group has real affinity traffic. New clients first fill groups with no Header activity in `client_affinity_dormant_seconds`, then groups below the traffic floor, before falling back to capacity-weighted rendezvous assignment.
- The active binding floor is enforced during automatic rebalance: an eligible group keeps at least `client_affinity_min_binding_per_group` Header-active client, and a last active client is never moved away merely to improve pressure. Persistent but dormant bindings remain in state and do not block a group from receiving a real active client.
- If regular Plus/Team groups are still missing their minimum binding floor, a standalone Pro/repeatable group is not selected as the ordinary rebalance target. Pro remains available for explicit affinity bindings and participates in normal capacity balancing after the regular floor is satisfied.
- Keeper API-key usage is attributed to a group only when the plugin observed a unique current-window Header route for that API key. API-key usage without Header evidence is shown as `api-key-only` for diagnostics and does not drive group balancing or subtract from auth-file usage.
- `client_affinity_api_key_map` provides a plugin-only fallback mapping from `X-CPA-Client-ID` to Keeper's numeric API-key ID when CPA does not populate scheduler metadata.
- Shared Pro backup usage is attributed only through API-key or recorded group activity. Unattributed remainder is marked partial and is not used for aggressive migration.
- Keeper errors, stale responses, missing auth usage, or unattributable shared-backup usage pause only the affected automatic migration; normal scheduling continues.
- An empty `auth_files` list is a valid no-traffic window: all group loads become zero, no binding moves, and the state records `no usage in analysis window` instead of an error.

## Resource UI

The resource UI is available at:

- `GET /v0/resource/plugins/quota-guard/status`

It shows account status, active quota windows, remaining quota percentages, refresh state, current primary/stale primary, affinity groups, and client bindings. Plus/Team rows show 5h first and Weekly second; group rows show both short-term availability and long-term Weekly capacity. Load cells keep the main Actual/Target/pressure summary visible and put verbose T+1 diagnostics in a hover tooltip; the Client Bindings, Rebalance Moves, and Analysis Log sections are collapsed by default.

Operational details:

- `Refresh All` and per-account `Refresh` refresh quota snapshots. They do not reset account status.
- `Client Bindings` is collapsed by default and sorted by `Last Seen` descending.
- `Delete Selected` removes mistaken or obsolete client bindings.
- `Move Selected` moves selected client bindings to an eligible target group. This is the supported manual rebalance path when persistent affinity leaves a group too full or too empty.
- `Analyze Now` refreshes Keeper load data and records a recommendation. `Rebalance Once` applies at most one guarded move.
- Background quota refresh and rebalance use separate loops. The Affinity section shows the latest Keeper collection attempt and completed analysis; analysis older than two configured rebalance intervals is flagged as stale.
- Quota refresh displays the last successful snapshot separately from the latest attempt. If the snapshot is older than `quota_snapshot_max_age_seconds` and the refresh failed, the account is explicitly marked `snapshot expired; refresh failed` with the returned error.
- When a stored affinity group has been removed from the current topology, the next request persistently rebinds that client to an eligible replacement group. Temporary quota or health failures still use request-scoped failover and retain the original binding.
- Group rows show rolling tokens, actual and capacity target shares, and normalized load factors. Actual binding changes are separated from analysis-only records; the page shows only the latest 20 rows in each section and keeps the full bounded history in state.
- Group rows also show minimum-floor state, Keeper API-key tokens, local fallback tokens, and attribution status. Client bindings show the recorded API-key ID and usage source when available.
- Resource UI actions do not require a management key by default; protect the route with network or reverse-proxy rules if it is exposed outside a trusted network.
- The page uses the same theme tokens as `management.html`, including `white`, `dark`, and `auto` theme support through the `cli-proxy-theme` browser setting.

## Requirements

- CLIProxyAPI with dynamic plugin support.
- Go 1.26 or newer.
- Linux build environment for the `.so` plugin artifact.
- Optional: CPA Usage Keeper when `quota_refresh_enabled` is used.

## Build

```bash
go test ./...
mkdir -p dist
go build -buildmode=c-shared -o dist/quota-guard.so .
```

## Install

Copy the built plugin into the CLIProxyAPI plugin directory:

```bash
cp dist/quota-guard.so /path/to/CLIProxyAPI/plugins/quota-guard.so
```

Then enable the plugin in the CLIProxyAPI config. A complete example is available in `config.example.yaml`.

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    quota-guard:
      enabled: true
      priority: 100
      state_file: "plugins/quota-guard-state.json"
      min_remaining_percent: 10
```

Restart CLIProxyAPI after replacing `quota-guard.so`; Go shared-library plugins are loaded into the running process and are not hot-reloaded.

## Local Development Against CLIProxyAPI

Released builds should use the published CLIProxyAPI module dependency from `go.mod`. While developing against a local CLIProxyAPI checkout, add a temporary local replace:

```bash
go mod edit -replace github.com/router-for-me/CLIProxyAPI/v7=/path/to/CLIProxyAPI
go test ./...
```

Remove the local replace before publishing:

```bash
go mod edit -dropreplace github.com/router-for-me/CLIProxyAPI/v7
go mod tidy
```

## Configuration

Start from `config.example.yaml` and merge the `plugins.configs.quota-guard` block into the CLIProxyAPI config. The most important operational options are:

- `min_remaining_percent`: quota floor before switching away from the current primary account.
- `quota_refresh_enabled`: enables real quota snapshot refresh through CPA/Keeper endpoints.
- `quota_refresh_endpoint`: per-auth refresh endpoint template. Supports `{auth_index}`, `{auth_id}`, and `{provider}`.
- `request_error_status_override_enabled`: keeps request-scoped client errors from unnecessarily moving traffic away from an otherwise healthy account.
- `client_affinity_enabled`: enables persistent `X-CPA-Client-ID` group affinity.
- `client_affinity_rebalance_enabled`: enables Keeper-backed load analysis.
- `client_affinity_rebalance_mode`: `observe` records recommendations; `auto` applies eligible moves.
- `client_affinity_rebalance_usage_endpoint`: Keeper realtime usage endpoint, normally using `window=60m`.
- `client_affinity_rebalance_fast_window_minutes` and `client_affinity_rebalance_fast_weight`: burst window and its prediction weight.
- `client_affinity_rebalance_overload_threshold`, `client_affinity_rebalance_target_threshold`, and `client_affinity_rebalance_overload_consecutive`: migration hysteresis.
- `weekly_budget_enabled`, `weekly_budget_timezone_offset_hours`, and `weekly_budget_max_adjustment_percent`: official Weekly T+1 budget control.
- `resource_actions_require_management_key`: controls whether resource-page actions require the management key.
- `proxy_restore_enabled`, `proxy_map_file`, `proxy_restore_interval_seconds`, `proxy_restore_settle_seconds`, and `proxy_restore_block_missing`: enable and tune post-login proxy restoration. Keep the map file private; status output shows only values such as `socks5://*:5556`.

## Management Routes

- `GET /v0/management/plugins/quota-guard/status`
- `GET /v0/management/plugins/quota-guard/config`
- `PATCH /v0/management/plugins/quota-guard/config`
- `POST /v0/management/plugins/quota-guard/refresh`
- `POST /v0/management/plugins/quota-guard/reset-window`

## Release Checklist

- Decide and add the project license.
- Run `go test ./...`.
- Build `dist/quota-guard.so`.
- Copy the artifact into a CLIProxyAPI plugin directory and restart CLIProxyAPI for a smoke test.
