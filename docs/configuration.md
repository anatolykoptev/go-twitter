# Configuration

## Accounts

An account needs one of: `AuthToken` + `CT0` from a browser session, a session saved in the `SessionStore`, or `Username` + `Password`. Session cookies are the supported way. Password login uses X's older login flow and is not tested against the current one.

`ParseAccounts("user:pass[:auth_token[:ct0[:totp_secret]]],...")` builds the slice from a comma-separated string.

## ClientConfig

`ClientConfig` fields (zero values take the defaults shown):

| Field | Default | Purpose |
|-------|---------|---------|
| `Accounts` | — | Accounts for the pool. |
| `DefaultProxy` | none | Proxy for the shared client and for accounts without their own `Proxy`. |
| `Account.Proxy` | none | Per-account proxy. A malformed one disables only that account (`account.proxy_client_failed`); `NewClient` fails only when no account is usable, so a single-account client still fails. |
| `SessionTTL` | 24h | How long the default file store reuses a saved session. Ignored by a custom `SessionStore`. |
| `SessionDir` | `~/.go-twitter/sessions` | Where the default file store persists sessions. Ignored when `SessionStore` is set. |
| `SessionStore` | file store | Where sessions are persisted; see [Session storage](sessions.md). |
| `AuthCooldown` | 1h | Soft-deactivation after auth errors. |
| `BanCooldown` | 6h | Soft-deactivation after ban or lock. |
| `NonResponsiveCooldown` | 5m | Base backoff for repeated non-200 failures (doubles, capped at 30m, ±30% jitter). |
| `CaptchaSolver` | none | Solver for login challenges and locked accounts (`captcha.NewCapsolver`). |
| `RateLimit` | 50 per 15m | Cap for endpoints without a measured seed. |
| `MaxInFlightPerAccount` | 2 | Max concurrent requests per account. Pooled picks skip accounts at the cap and wait (bounded by the caller's ctx, on the same `pool.wait` machinery as endpoint limits) when every eligible account is full; the account-pinned POST and media paths wait for their account's slot. A negative value disables the cap. See [Rate limiting](rate-limits.md). |
| `AccountPaceMin` / `AccountPaceRandom` | 800ms / 1.2s | Per-account request spacing. |
| `AccountPaceDisabled` | false | Turn per-account pacing off. |
| `ProxyBackoffInitial` / `ProxyBackoffMax` | 30s / 30m | Backoff for failing proxies. |
| `OpenAccountCount` | 0 | Anonymous guest sessions to create at startup. |
| `DisableGuestFallback` | false | Refuse the guest-token fallback on endpoints that allow it. |
| `PoolAlertHook` | log a warning | `func(topic string, payload any)` called on pool events. This repo emits `proxy.down` (user, masked proxy, consecutive failures, backoff) and `xtid.init_failed`; the account pool emits its own topics, such as account deactivation. `account.proxy_client_failed` (a per-account proxy client failed to build; payload keys `user`, `proxy` (masked label), `error` (fixed text)). |
| `MeterProvider` | global OpenTelemetry provider | Provider for the client's metrics; see [metrics](metrics.md). |
| `RequestJitter` | none | Optional delay before pooled requests; see `WithRequestJitter` in [request format](request-format.md). |
| `XtidKeySource` | account page | Where transaction-id keys come from; see [request format](request-format.md). |
| `OnRequest` | nil (no-op) | `func(RequestEvent)` called once per pooled-account HTTP attempt after classification — the request observer; see [Rate limiting](rate-limits.md). Must not block. |

## Environment variables

| Variable | Purpose |
|----------|---------|
| `GO_TWITTER_XTID_KEY_SOURCE` | `logged-out` selects the same key source as `XtidKeySource`, including for shared managers. |
| `TWITTER_QID_<OPERATION>` | Overrides the GraphQL operation ID for one operation, for example `TWITTER_QID_USER_TWEETS`. |
| `TWITTER_QID_REFRESH` | `off` disables the runtime operation ID refresh. |
| `TWITTER_QID_XWEB` | `on` lets the refresh also read the x-web bundle graph. See [x-web migration](xweb-migration.md). |
| `TWITTER_DUMP_DIR` | Debugging only. Writes X's response bodies to this directory, so treat it as sensitive data. |
