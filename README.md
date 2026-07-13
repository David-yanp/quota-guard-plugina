# Quota Guard Plugin

Quota Guard is a standalone scheduler, usage, and management plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It implements quota-aware `fill-first` scheduling with a configurable safety reserve: accounts are considered in priority order, but the plugin moves away from the current account before it reaches the configured quota floor.

This repository is published independently from CLIProxyAPI so the plugin can be updated or released without carrying local patches in the upstream server source tree.

## Behavior

- Applies to every scheduler candidate supplied by the host. It does not filter by provider.
- Sorts candidates by highest priority, then stable auth ID order.
- Ignores disabled, unavailable, or status-disabled candidates.
- Creates default state for new accounts and treats them as 100% remaining.
- Tracks 5h and 7d windows by default. Codex accounts can also use real `codex_quota` snapshots from auth JSON; monthly is enabled when the snapshot, manual calibration, or an external quota query provides it.
- Deducts usage and inflight reservations after the latest real quota snapshot so scheduling can react between refreshes.
- Records the currently selected account as `primary` after `scheduler.pick`.
- Can refresh Codex quota snapshots through a CPA/keeper endpoint template such as `/cpa/api/v1/quota/refresh/{auth_index}`.
- Treats narrow request-scoped failures (`context_too_large`, `invalid_request_error`, `context canceled`) as non-credential failures when the host still passes the auth as a candidate, no unavailable flag is set, and no future cooldown exists. Credential/provider failures such as 401, 429, Cloudflare challenge, unauthorized, quota, and rate-limit messages remain ineligible.
- Selects the first candidate whose lowest active-window remaining percent is at or above `min_remaining_percent`.
- Returns a retryable scheduler error when all eligible candidates are below the threshold if `fail_when_all_low` is true.
- Otherwise delegates to `delegate_when_unconfigured`, usually `fill-first`.

## X-CPA-Client-ID Affinity

When `client_affinity_enabled` is true, requests with `X-CPA-Client-ID` are bound to an affinity group. The binding is persistent so the same client normally keeps using the same group until that group has no eligible member.

- Missing `X-CPA-Client-ID` keeps the legacy/global reserve-until-low fill-first behavior.
- Automatic groups use Plus/Team accounts as the main member and Pro/repeatable accounts as backups.
- Group IDs are stable for the main account, for example `auto-<auth_index>`.
- The current assignment algorithm is not a hash-to-group algorithm. New clients are assigned to the eligible group with the lowest `binding_count / group_weight` score. Existing bindings stay where they are unless the group becomes unavailable or an operator moves/deletes them.
- Pro/repeatable backup accounts may appear in multiple groups, but should not receive group primary traffic while the main member is eligible above reserve.

### Smooth Load Rebalancing

Quota Guard can combine Keeper's rolling auth-file usage with plugin-side client activity to rebalance persistent bindings without disrupting active sessions.

- Keeper `current_usage.auth_files` is authoritative for group-level tokens and requests.
- Client pick and usage activity is retained only for the configured rolling window and cooldown periods.
- Loads are normalized by the main account capacity; shared Pro backups are not counted as group capacity multiple times.
- Only clients used during the last hour and idle for at least 10 minutes are eligible to move. Completely unused bindings stay unchanged.
- Automatic moves are limited to one per cycle, followed by a 60-minute cooldown. Manual moves receive a 24-hour cooldown.
- `observe` mode records recommendations without changing bindings. `auto` mode applies moves that satisfy the load-ratio and predicted-improvement guards.
- Keeper errors, stale responses, missing auth usage, or unattributable shared-backup usage fail closed and never interrupt normal scheduling.

## Resource UI

The resource UI is available at:

- `GET /v0/resource/plugins/quota-guard/status`

It shows account status, active quota windows, remaining quota percentages, refresh state, current primary/stale primary, affinity groups, and client bindings.

Operational details:

- `Refresh All` and per-account `Refresh` refresh quota snapshots. They do not reset account status.
- `Client Bindings` is collapsed by default and sorted by `Last Seen` descending.
- `Delete Selected` removes mistaken or obsolete client bindings.
- `Move Selected` moves selected client bindings to an eligible target group. This is the supported manual rebalance path when persistent affinity leaves a group too full or too empty.
- `Analyze Now` refreshes Keeper load data and records a recommendation. `Rebalance Once` applies at most one guarded move.
- Group rows show rolling tokens, actual and capacity target shares, and normalized load factors. Rebalance history remains collapsed by default.
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
- `resource_actions_require_management_key`: controls whether resource-page actions require the management key.

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
