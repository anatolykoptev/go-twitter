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
| `twitter.client.pool.wait.duration` | histogram | `s` | `twitter.operation`, `twitter.result` |
| `twitter.client.relogins` | counter | `{relogin}` | `twitter.outcome` |
| `twitter.client.proxy.failures` | counter | `{failure}` | none |

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
| `auth` | code 32 / 179 / 219, or a bare 401 |
| `csrf` | code 353 |
| `banned` | code 88 with `x-rate-limit-remaining` > 0 (a declared ban; see the escalation note below) |
| `suspended` | code 64 |
| `locked` | code 326 |
| `blocked` | code 161 |
| `forbidden` | HTTP 403 without a recognized code |
| `internal` | code 131 (recorded as `internal` even when the client then accepts usable data) |
| `not_found` | HTTP 404 |
| `transport` | the request failed before an HTTP response (timeout, reset, DNS on a direct account, ...) |
| `proxy` | transport failure that looks like a proxy failure (`isProxyError`) on an attempt made through a per-account proxy; the same gate as `proxy.failures` |
| `other` | anything else (e.g. 5xx) |

**Header-less-88 escalation.** Three consecutive code-88 responses carrying no
parseable `x-rate-limit-remaining` (with no successful response in between) are
a ban signature: the account is soft-deactivated for `BanCooldown` and the call
fails with an `IsBanned` error. Each of those responses is still recorded here
as `rate_limited` — this instrument reports the per-response truth, and a
header-less 88 is per-response a rate limit — so the escalation is **not**
visible as a `banned` outcome. It is signalled instead by the
`account banned (repeated header-less code 88)` log warning, a non-rate-limited
`MetricsHook` call on the escalating response, and the pool's
`alerts.item_deactivated` alert.

Buckets (seconds): `0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30` - sized for a
50 ms to 30 s HTTP call. The duration covers the HTTP round trip only, not the
anti-fingerprint jitter, per-account pacing or retry backoff.

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

### `twitter.client.pool.wait.duration`

One record per call that actually waited inside `nextAccountWithWait` — the
sleep taken when every pool account is momentarily unusable for the endpoint
(all active but endpoint-rate-limited or in proxy backoff). Calls served
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
