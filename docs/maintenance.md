# Maintenance

## GraphQL operation IDs (gql-sync)

x.com rotates GraphQL queryIDs and feature-flag defaults inside its JS bundle; a stale ID or flag set returns HTTP 400. `cmd/gql-sync` extracts the current values from x.com's live bundles and regenerates `queryids_gen.go` and `features_gen.go`. At runtime the effective queryID for an operation is resolved as environment override (`TWITTER_QID_*`) over runtime-refreshed over generated over committed literal.

**Runtime refresh.** A process that holds an authenticated account also re-reads the IDs from x.com's own bundle (the authenticated page it already fetches for `x-client-transaction-id`), so an X rotation heals without a release. A refresh runs in the background, on the first authenticated request, after 6 hours, and after 3 endpoint faults (HTTP 404 / GraphQL-shaped 400) on one operation within 2 minutes; refreshes are at least 15 minutes apart, and consecutive failed or rejected refreshes back off exponentially (15 min, 30 min, ... capped at 24 h; a success resets it; the fault trigger obeys it too). A later refresh overlays the earlier one: ids it did not find are carried forward, never reverted. The whole extraction is validated (every ID must match `^[A-Za-z0-9_-]{1,64}$`, at least 40% of the known operations must be present, a malformed ID rejects it all) and swapped in atomically; on any failure the last good IDs stay and one `queryid.refresh_failed` pool alert fires per ok-to-failed transition (the hook is process-wide: the last `NewClient` that sets `PoolAlertHook` owns it). The request that hit the fault is not retried — the next call uses the new ID. Feature flags are only added to, never changed or removed. `Endpoints[op]` keeps the compiled value; the layered value is what requests use (`EndpointURL`). Set `TWITTER_QID_REFRESH=off` to disable. Metrics: [Metrics](metrics.md). Operations that are not reachable from the public bundles (the authenticated read cluster and the like/retweet mutations) are maintained by hand, with the env override as the hotfix path.

```bash
go run ./cmd/gql-sync -proxy "$PROXY_URL" -fail-on-empty
# logged-out x.com serves a shell without the bundle; pass an account's cookies
# via the environment (never as a flag; sent to x.com and twitter.com hosts only, over https, never logged):
GQL_SYNC_COOKIES='auth_token=...; ct0=...' go run ./cmd/gql-sync -fail-on-empty
```

Flags: `-out` (repo root, default `.`), `-proxy`, `-fixtures` (read from a directory instead of the network), `-date`, `-fail-on-empty` (exit non-zero instead of leaving the files untouched when nothing is extracted).

## Tools

- `cmd/xtid-probe` checks live which transaction-id key source X accepts.
- `cmd/xweb-report` regenerates the [x-web migration inventory](xweb-migration.md).

## CI

**gql-sync drift check** (`.github/workflows/gql-sync-drift.yml`) — runs weekly
(Mon 06:00 UTC, plus manual `workflow_dispatch`). It runs gql-sync against x.com's **live** JS
bundle and, if the generated files changed, opens a PR for human review (it never auto-merges
generated values — a wrong queryID/flag default is an HTTP 400 on every call).

Requires repo secret **`TWITTER_PROXY`**: a proxy URL that can reach x.com from
GitHub's runners. The job runs with
`-fail-on-empty`, so a missing proxy or a bundle break exits non-zero and fails
closed instead of overwriting the committed IDs with nothing.

**preflight** (`.github/workflows/preflight.yml`) — `make preflight` (gofmt, vet, build, test) on every PR and push to `main`.
