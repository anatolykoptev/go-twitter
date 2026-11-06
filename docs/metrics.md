# Metrics

go-twitter emits [OpenTelemetry](https://opentelemetry.io/) metrics through the
OpenTelemetry **metric API** only. The library never imports an SDK or exporter:
the application installs a `MeterProvider` (Prometheus, OTLP, ...) and passes it
in `ClientConfig.MeterProvider`. When it is nil the client uses
`otel.GetMeterProvider()`, which is a no-op until the application sets a global
provider, so an unconfigured client costs nothing.

Instrumentation scope: `github.com/anatolykoptev/go-twitter`, versioned with the
module version seen by the consuming binary.

If an instrument cannot be created the client logs a warning once and keeps
running with a no-op instrument; client construction never fails because of
metrics.

## Instruments

| Name | Type | Unit | Attributes |
|---|---|---|---|
| `twitter.client.request.duration` | histogram | `s` | `twitter.operation`, `twitter.outcome` |
| `twitter.client.timeline.freshness` | histogram | `s` | `twitter.operation` |
| `twitter.client.pool.accounts` | observable gauge | `{account}` | `twitter.account.state` |
| `twitter.client.pool.inflight` | observable gauge | `{request}` | none |
| `twitter.client.pool.wait.duration` | histogram | `s` | `twitter.operation`, `twitter.result` |
| `twitter.client.relogins` | counter | `{relogin}` | `twitter.outcome` |
| `twitter.client.proxy.failures` | counter | `{failure}` | none |
| `twitter.client.proxy.client_failures` | counter | `{failure}` | none |
| `twitter.client.automated.shared_suspect` | counter | `{signal}` | none |
| `twitter.client.ratelimit.remaining` | histogram | `{request}` | `twitter.operation` |
| `twitter.client.ratelimit.limit` | histogram | `{request}` | `twitter.operation` |
| `twitter.client.ratelimit.holds` | counter | `{hold}` | `twitter.operation` |
| `twitter.client.queryid.refreshes` | counter | `{refresh}` | `twitter.trigger`, `twitter.result`, `twitter.layout` |
| `twitter.client.queryid.changed` | gauge | `{id}` | none |
| `twitter.client.queryid.age` | gauge | `s` | none |
| `twitter.client.queryid.refresh.duration` | histogram | `s` | `twitter.trigger`, `twitter.result` |
| `twitter.client.xtid.failures` | counter | `{failure}` | none |
| `twitter.client.xtid.refreshes` | counter | `{refresh}` | `twitter.result` |
| `twitter.client.xtid.retries` | counter | `{retry}` | `twitter.result` |
| `twitter.client.usage_report.dropped` | counter | `{report}` | none |
| `twitter.client.parse.dropped` | counter | `{entry}` | `twitter.operation`, `twitter.reason` |
| `twitter.client.parse.created_at_fallbacks` | counter | `{tweet}` | `twitter.operation` |
| `twitter.client.pagination.pages` | histogram | `{page}` | `twitter.operation` |
| `twitter.client.pagination.stops` | counter | `{call}` | `twitter.operation`, `twitter.reason` |

No instrument ever carries an account handle, user ID, list ID, search query or
URL as an attribute.

### `twitter.client.request.duration`

One record per HTTP request attempt to X in the pool GET/POST, account-pinned
POST and guest-fallback paths. This includes the retries the client makes inside
one call (ct0 recovery, post-relogin retry), so a single `GetUserTweets` that
hits a CSRF error records two points. Media upload and `ValidateAccount` are not
instrumented.

- `twitter.operation`: the GraphQL operation / endpoint name (`UserTweets`,
  `SearchTimeline`, `ListLatestTweetsTimeline`, ...), bounded by the `Endpoints`
  registry plus the fixed REST names `DMInbox` and `SendDM`.
- `twitter.outcome` is the truth about that one HTTP attempt, **not** call-level
  success: a call that eventually succeeds after a retry still records the failed
  attempts, and a code-131 response that the client accepts because it carries
  usable data is recorded as `internal`, not `ok`. Derived from the same classifier the retry loop uses
  (`classifyError` / `newAPIError`), so it cannot drift from the error taxonomy:

| Value | Meaning |
|---|---|
| `ok` | 2xx, no error code in the body |
| `rate_limited` | HTTP 429, or code 88 with `x-rate-limit-remaining` 0 or absent |
| `auth` | code 32 / 37 / 179 / 219, or a bare 401 |
| `csrf` | code 353 |
| `banned` | code 88 with `x-rate-limit-remaining` > 0 (a declared ban; see the escalation note below) |
| `suspended` | code 64 |
| `locked` | code 326 |
| `blocked` | code 161 |
| `automated` | code 226 — X flagged the request as possibly automated; the account is benched (`AuthCooldown`, then `BanCooldown` on a repeat within 24 h) |
| `forbidden` | HTTP 403 without a recognized code |
| `internal` | code 131 (recorded as `internal` even when the client then accepts usable data) |
| `not_found` | two populations under one label: HTTP 404 — the historical dead-route label (an endpoint fault under the hood: never charged to the account) — and an errors-only 2xx carrying a deterministic-miss marker (`ErrNotFound`, an absent resource). It is no longer the raw 404 rate; an alert on it counts both |
| `endpoint` | HTTP 400 with a GraphQL-shaped `errors` body — an endpoint-wide fault (stale features / request shape); never charged to the account |
| `bad_request` | HTTP 400 with a GraphQL error entry carrying code `214` (top-level or inside `extensions`) — a deterministic caller-input rejection (`ErrBadRequest`): never retried, rotated or charged to the account, and not an endpoint fault |
| `unexpected` | an errors-only 2xx whose entries match no known class and no miss marker (`ErrUnexpectedResponse`): X answered with errors the library does not recognise — terminal, never charged to the account, and not an endpoint fault |
| `overloaded` | HTTP 5xx, or a `LoadShed` error entry (code -1 / message containing "LoadShed"); retried on another account, never charged to the serving one |
| `transport` | the request failed before an HTTP response (timeout, reset, DNS on a direct account, ...) |
| `proxy` | transport failure that looks like a proxy failure (`isProxyError`) on an attempt made through a per-account proxy; the same gate as `proxy.failures` |
| `other` | anything else (e.g. an unshaped 4xx) |

**Header-less-88 escalation.** Three consecutive code-88 responses carrying no
parseable `x-rate-limit-remaining` (with no successful response in between) are
an account restriction: the account is soft-deactivated for `BanCooldown` and the call
fails with an `IsBanned` error. Each of those responses is still recorded here
as `rate_limited` — this instrument reports the per-response truth, and a
header-less 88 is per-response a rate limit — so the escalation is **not**
visible as a `banned` outcome. It is signalled instead by the
`account banned (repeated header-less code 88)` log warning, a non-rate-limited
`MetricsHook` call on the escalating response, and the pool's
`alerts.item_deactivated` alert.

Buckets (seconds): `0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30` - sized for a
50 ms to 30 s HTTP call. The duration covers the HTTP round trip only, not the
opt-in request jitter, per-account pacing or retry backoff.

### `twitter.client.timeline.freshness`

For every successful tweet-timeline response (`UserTweets`, `SearchTimeline`,
`ListLatestTweetsTimeline`, `HomeTimeline`, `HomeLatestTimeline`,
`CommunityTweetsTimeline`, `Bookmarks`; one record per page for paginated
operations) the client records `now - createdAt(newest tweet in the response)`,
ignoring promoted (ad) entries - their IDs are minted at serve time and would
mask staleness - where `createdAt` is decoded from the tweet's snowflake ID
(`(id >> 22) + 1288834974657` ms).

**Semantics.** This is the age of the freshest item in the response. For a
high-volume timeline it is an upper bound on pipeline lag (X publication to our
retrieval): the true lag cannot exceed it, but it can be lower. It is **not** a
per-tweet delivery latency, and for a quiet timeline it simply measures how long
ago the last tweet was posted. It also depends on the local clock; a negative age
(local clock behind X) is recorded as `0`. For `Bookmarks` the order is bookmark
time, not tweet time, so the value is the age of the newest bookmarked tweet and
is rarely meaningful. For `SearchTimeline` the Top tab is relevance-ranked, but
the newest tweet in the response is still what is measured.

To compute your own per-tweet first-seen lag use the exported helper:

```go
created, err := twitter.SnowflakeTime(tweet.ID)
lag := time.Since(created)
```

Buckets (seconds): `0.5, 1, 2, 3, 5, 8, 12, 20, 30, 45, 60, 120, 300, 900, 3600`
- dense across 0.5 s to 60 s, with a coarse tail for quiet timelines.

### `twitter.client.ratelimit.remaining` / `twitter.client.ratelimit.limit`

What X says about the rate-limit window of an operation, taken from the
`x-rate-limit-remaining` and `x-rate-limit-limit` response headers. One record
per HTTP response that carries the header, on the pool GET/POST, account-pinned
POST (writes) and media-upload paths, for every status (200, 429, 4xx). A
response without the header records nothing. Guest-fallback and recovery
sub-requests (ct0 rotation, relogin retry) are not recorded.

- `twitter.operation`: same bounded endpoint label as `request.duration`. There
  is no account attribute: the library never labels by account handle, and
  per-account detail is not needed to learn the per-operation limit.
- `limit` is the per-account, per-endpoint ceiling of one window, so its
  distribution collapses to the real limit X enforces for that operation
  (`max` over a window is the number to read). It is what to use to retire the
  hand-measured `endpointDefaultLimit` table.
- `remaining` shows how close polling runs to the ceiling: a mass in the `0`-`2`
  buckets means accounts are being spent to exhaustion (and held back until
  reset, see README), a mass near `limit` means headroom is unused.

Buckets (requests): `0, 1, 2, 5, 10, 25, 50, 100, 150, 200, 300, 500`.

### `twitter.client.ratelimit.holds`

Incremented each time a response shows an endpoint's window spent
(`x-rate-limit-remaining` at or below the headroom, reset still ahead) and the
endpoint is taken out of rotation on that account until the reset. It is the
proactive counterpart of a 429: a rising `holds` with a flat
`request.duration{outcome="rate_limited"}` means the headers are keeping accounts
out of 429s. Pool paths only; writes and media uploads pinned to an account
never hold. Counted at the response that triggered the hold, not for the
pick-time estimate hold.

### `twitter.client.xtid.failures`
Incremented for every request whose `x-client-transaction-id` could not be generated
(current-wire requests then carry the web client's failure value, legacy ones go
out without the header); the keys could not be built or the init is in its
negative-cache backoff. The request is not blocked; a sustained non-zero rate
means the generator is broken and X will likely answer 404s. The client also logs
one warning per failure streak, at most once every 5 minutes.

### `twitter.client.xtid.refreshes`
One count per forced rebuild attempt of the transaction-id generator, made when a
request is answered 404 on a known route (current-wire accounts only), by
`twitter.result`: `refreshed` (new keys in place), `suppressed_floor` (nothing
rebuilt: keys younger than the one-minute floor, no keys yet, or the attempt kind
is in backoff), `failed` (the rebuild failed; the previous keys keep serving) and
`cancelled` (the caller's context ended while waiting). A steady `refreshed` rate
with `retries{result="still_404"}` means the 404s are not about the id.

### `twitter.client.xtid.retries`
Outcome of the single re-send after a `refreshed` rebuild, by `twitter.result`:
`rescued` (the call then succeeded) or `still_404` (the retry was answered 404
again and took the ordinary endpoint-fault path). A call whose retry ends some
other way (429, transport error) counts neither.

### `twitter.client.usage_report.dropped`
`*WithSocial` helpers send their usage report to the credential service in the
background, at most 16 in flight. A report that arrives when all 16 slots are
busy (the service is slow or stuck) is dropped, and this counter is incremented.
A sustained non-zero rate means the credential service is not keeping up and its
view of lease health is lagging. Reported on the `SetSocialMeterProvider` provider.

### `twitter.client.parse.dropped`

One count per timeline entry the parser dropped, or flagged, instead of silently
returning it. Before this counter these cases were a debug log line only, so a
parse that went empty looked like "no new tweets".

- `twitter.operation`: the operation whose response was parsed.
- `twitter.reason`:
  - `no_id`: a tweet result with no `rest_id`.
  - `no_created_at`: a tweet with an id but no creation time from `legacy.created_at`,
    `details.created_at_ms` or the snowflake id. The tweet is dropped.
  - `unhandled_type`: a timeline item whose `__typename` the parser does not know
    (a new X entry kind). Prompts, user items and cursors are expected and not counted.
  - `tombstone`: a deleted or unavailable tweet. Normal at a low rate.
  - `blank_text`: NOT a drop, the tweet is returned. Its text is empty in `legacy`,
    `details` and `note_tweet`, and it has no media, card, quote or retweet to
    explain that: a likely blanked parse.

Which reasons each feed reports:

| Feed (`twitter.operation`) | Reasons |
|---|---|
| `UserTweets`, `SearchTimeline`, `TweetDetail`, `Bookmarks`, `ListLatestTweetsTimeline`, `CommunityTweetsTimeline`, `HomeTimeline`, `HomeLatestTimeline` | `no_id`, `no_created_at`, `unhandled_type`, `tombstone`, `blank_text` |
| `tweetNotificationsQuery` (x-web feed) | `no_id`, `no_created_at` |
| `DeviceFollow` (REST feed) | `no_id` |

A sustained rate of `no_id`, `no_created_at`, `unhandled_type` or `blank_text`
means X changed the response shape. A UserTweets response that carries no timeline
at all is not counted here: it fails the call with `ErrUnexpectedResponse`.

### `twitter.client.parse.created_at_fallbacks`

One count per tweet whose creation time came from its snowflake id because no
`created_at` field carried one (`legacy.created_at`, `details.created_at_ms`).
The tweet is kept and is not a drop; the time is exact (the id encodes it). A
sustained rate means X moved the timestamp field. Attribute: `twitter.operation`,
reported by the timeline reads, `tweetNotificationsQuery` and `DeviceFollow`.

### `twitter.client.pagination.pages` / `.stops`

One record per finished call of a cursor-paginated read: `GetFollowers`, `GetFollowing`,
`GetVerifiedFollowers`, `GetRetweeters`, `GetListTweets`, `GetCommunityTweets`,
`GetBookmarks`. `twitter.operation` is the GraphQL operation (`Followers`, `Following`,
`BlueVerifiedFollowers`, `Retweeters`, `ListLatestTweetsTimeline`,
`CommunityTweetsTimeline`, `Bookmarks`).

- `pages`: requests the call made, counting one that failed. A healthy read takes
  `ceil(max / page size)` pages or fewer; a distribution far above that means lists
  are being paged to their end instead of stopping.
- `stops`: why the call stopped paging, `twitter.reason`:
  - `max_count`: the requested number of items was collected.
  - `no_cursor`: the page carried no Bottom cursor, the natural end.
  - `empty_page`: the page held no items although a cursor came with it.
  - `repeated_cursor`: the cursor was one already followed in this call.
  - `terminal_cursor`: the cursor is X's end-of-timeline marker (`0|...` or a bare `0`).
  - `page_cap`: 5 consecutive pages added no new item (items are deduplicated by id within the call).
  - `ctx`: the caller's context ended.
  - `error`: a request or parse failed.

`empty_page`, `repeated_cursor`, `terminal_cursor` and `no_cursor` are normal ends; a
sustained `page_cap` rate means pages keep arriving with nothing new, which is worth a look at
the raw response. `page_cap` and `terminal_cursor` calls that end short of `max` also log a warning. A `ctx` share that dominates means callers'
deadlines are shorter than the reads they ask for.

### `twitter.client.queryid.refreshes` / `.changed` / `.age`

The runtime queryId/feature refresh (README, "GraphQL ID maintenance"). The state
is process-wide, so these describe the process, not one client; they are
recorded through whichever client triggered or observed the refresh, so the
`*WithSocial` helpers report them on the `SetSocialMeterProvider` provider.

- `refreshes`: one count per refresh attempt that ran, plus `suppressed` counts of due
  attempts the spacing refused.
  - `twitter.trigger`: `first` (no successful refresh yet), `ttl` (the active map
    outlived 6 h), `fault` (3 endpoint faults on one operation within 2 min).
  - `twitter.result`: `ok`; `ok_xweb_only` (a success from an x-web-only page: with
    `TWITTER_QID_XWEB=on`, the Vite-only page's legacy chunks were all fetched and held
    no legacy operation, so only the x-web native ids were applied; it also logs
    `x-web-only page`); `fetch_failed` (the page or no bundle could be read:
    network, logged-out shell, no x-web scripts); `rejected` (read, but failed
    validation, so nothing was applied); `panic` (the walk panicked on a bundle body and was
    recovered); `suppressed` (an attempt was due but the 15 min / failure-backoff spacing refused it;
    not a refresh, counted so a stuck backoff is visible).
  - `twitter.layout`: the layout of the authenticated warm page the attempt read:
    `webpack` (the legacy responsive-web build), `vite` (the x-web build),
    `vite+webpack` (a page showing both), or `unknown` (no page could be read, or it
    carried no recognisable script). A `suppressed` attempt reads no page, so it
    carries the last recognised layout. A sustained `layout!="webpack"` means the
    signed-in page is no longer the legacy build.
  - A `fetch_failed` or `rejected` result keeps the last good map. The first
    failure after a success (or after process start) also fires one
    `queryid.refresh_failed` pool alert; further failures do not until a success
    in between. Alert on `rate(refreshes{result!~"ok|ok_xweb_only"})` for a persistent break.
  - `ok_xweb_only` means the authenticated page is already the x-web build: only the
    x-web native operations were refreshed, every other id keeps its last value (it
    is not healed). Treat it as the signal that the build changed, not as a plain
    success.
  - Layout alert: the first time a process sees the page move from `webpack` to `vite`
    or `vite+webpack`, one `queryid.layout_changed` pool alert fires (payload `from`,
    `to`), through the same hook as `queryid.refresh_failed`. It fires once per such
    transition, never for the first page a process sees, and is fed by both the xtid
    warm-page hook and the refresher's own page fetch (the only source when xtid does
    not fetch the authenticated page). An unrecognised page, and the logged-out shell (served when an account's cookies are
    refused), change nothing: no alert, and the stored layout and the `twitter.layout` label
    keep the last known value. The alert needs a previous layout, so a flip that happens while
    the process is down raises no alert; the `twitter.layout` label still shows it.
- `changed`: how many operation IDs differed from the compiled set on the last
  successful refresh (operations pinned by a `TWITTER_QID_*` override are not
  counted: the refreshed id is not applied to them). A non-zero value means the binary is behind X: rebuild with
  `cmd/gql-sync` output. Zero after a success means the compiled IDs are current.
- `refresh.duration`: wall time of one attempt (page + chunk walk), buckets 1 s to 180 s
  (the walk timeout). Every attempt also logs `queryid refresh finished` at INFO with the same
  duration. A later refresh overlays the earlier: ops it did not find keep their earlier id, and the
  count carried forward is the `carried_forward` field of the `queryid refresh applied` log.
- `age`: seconds since the active map was last refreshed, or since process start
  while the compiled IDs are still active. Sampled on every authenticated
  request, so it is only fresh while traffic flows. With traffic, an age beyond
  the 6 h TTL plus the 15 min minimum interval means refreshes are failing.

### `twitter.client.pool.accounts`

Observed on collection. It carries no per-client attribute, so use **one client
per `MeterProvider`** for this gauge: two clients on the same provider report the
same series and the last callback wins. Call `Client.Close()` to unregister the
callback when a client is discarded (idempotent). Every pool account is in exactly one state, so the series
sum to the pool size; all four states are always reported (zeros included).

| `twitter.account.state` | Meaning |
|---|---|
| `active` | usable |
| `proxy_backoff` | active, but its proxy is in exponential backoff |
| `cooling_down` | soft-deactivated (auth/ban/failure cooldown); the pool reactivates it automatically |
| `deactivated` | permanently deactivated (suspended) |

### `twitter.client.pool.inflight`

Observed on collection, alongside `pool.accounts` in the same callback (same
one-client-per-provider rule). The sum of every pool account's in-flight
request count — the requests currently holding a slot under
`ClientConfig.MaxInFlightPerAccount` (default 2, negative disables the cap).
The gauge counts slots, not sockets: a cancelled request releases its slot at once while its socket can outlive it until the transport's own timeout. Each account's count never exceeds the cap; a saturated pool waits on the
`pool.wait` machinery instead of exceeding it, so a persistently pegged gauge
means the cap or the account count is the bottleneck. Per-call `*WithSocial`
clients register no gauge.

### `twitter.client.pool.wait.duration`

One record per call that actually waited inside `nextAccountWithWait` — the
sleep taken when every pool account is momentarily unusable for the endpoint
(all active but endpoint-rate-limited, in proxy backoff, or at the
`MaxInFlightPerAccount` cap). Calls served
without waiting record nothing, so the series only exists while the pool is
contended. The value is the total time the call slept, which can add up to
minutes of latency per request (bounded by `poolWaitCap`, 5 min). The wait
inside `pool.NextWithWait` itself (all accounts deactivated) is not covered —
that path returns before the wait this histogram measures begins.

- `twitter.operation`: same bounded endpoint label as `request.duration`.
- `twitter.result` is how the waited call ended:

| Value | Meaning |
|---|---|
| `acquired` | the wait ended with a usable account |
| `gave_up` | waited, then no account could serve the endpoint within the wait budget |
| `ctx_done` | the caller's context (cancel or deadline) ended the wait |

Buckets (seconds): `0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300` —
same resolution as `request.duration` up to 30 s, extended to the 5-minute
wait cap.

### `twitter.client.relogins`

Automatic relogin attempts. `twitter.outcome` is `ok`, `failed`, or
`breaker_open` (the relogin gate - the built-in per-account breaker or one set
with `SetAutoReloginGate` - refused the attempt).

### `twitter.client.proxy.failures`

Incremented at the same site that emits the `proxy.down` pool alert: a
proxy-layer failure that puts an account's proxy into backoff.

### `twitter.client.automated.shared_suspect`

Incremented once each time the number of DISTINCT accounts that answered code 226 within the last 10 minutes reaches 2 or more (and again only after it has dropped below 2). Fired alongside one error log line and the `automated.shared_suspect` pool alert. A shared cause (proxy, request signature) is suspected; the signal is detection only.

### `twitter.client.proxy.client_failures`

Incremented once per account in `NewClient` whose pinned per-account proxy
client could not be built (e.g. a malformed proxy URL). That account is
disabled for the lifetime of the Client (state `deactivated`, never
reactivated, never sent through the shared client) and the
`account.proxy_client_failed` pool alert fires. `NewClient` still succeeds as
long as one account is usable.

## The `*WithSocial` helpers

`SearchWithSocial`, `GetUserTweetsWithSocial` and the other `*WithSocial`
helpers take no `ClientConfig`; each call builds a short-lived one-account
client. Supply the `MeterProvider` once, at start-up:

```go
twitter.SetSocialMeterProvider(mp) // nil restores otel.GetMeterProvider()
```

Per-call clients record `request.duration`, `timeline.freshness`, `relogins`
and `proxy.failures` to that provider. They deliberately do **not** register
`twitter.client.pool.accounts`: the gauge has no per-client attribute, so a
one-account per-call client would overwrite the series of a long-lived pool.
Each per-call client is closed when its call returns. If you also run a
long-lived `NewClient`, give it the same provider; its gauge is unaffected.

## Wiring a Prometheus exporter

```go
import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/anatolykoptev/go-twitter"
)

exporter, _ := prometheus.New()
mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
client, _ := twitter.NewClient(twitter.ClientConfig{Accounts: accounts, MeterProvider: mp})
http.Handle("/metrics", promhttp.Handler())
```

Prometheus names become `twitter_client_request_duration_seconds_bucket{twitter_operation="UserTweets",twitter_outcome="ok"}`
and so on. The p50 publication-to-retrieval upper bound for a list:

```promql
histogram_quantile(0.5, sum by (le) (rate(twitter_client_timeline_freshness_seconds_bucket{twitter_operation="ListLatestTweetsTimeline"}[5m])))
```

## Migrating from `MetricsHook`

`ClientConfig.MetricsHook` still works exactly as before and is deprecated: it
carries no latency or error class. It can be removed once dashboards use the
instruments above.
