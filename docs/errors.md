# Error handling

Every request makes up to 3 attempts, rotating accounts on pool-based calls. Recovery per error class:

| Error | Code | Action |
|-------|------|--------|
| CSRF mismatch | 353 | Adopt the ct0 from the error response's `set-cookie` and retry. If x.com offers none, or the retry fails, relogin (subject to the breaker) and retry once. A random ct0 is never generated. |
| Auth expired | 32 | Relogin (subject to the breaker) and retry; on failure soft-deactivate the account for `AuthCooldown`. |
| Rate limited | 429 | Mark the endpoint rate-limited for that account until `x-rate-limit-reset` (15 minutes if absent) and try another account. A 200 whose `x-rate-limit-remaining` is already spent holds the endpoint the same way, before any 429 (see [Rate limiting](rate-limits.md)). |
| Rate limited (in a 200 body) | 88, `x-rate-limit-remaining` 0 | Same as 429: mark only that endpoint rate-limited for that account until `x-rate-limit-reset` (15 minutes if absent; a reset in the past is raised to 1 second from now). The account stays active. |
| Rate limited, header-less (in a 200 body) | 88, `x-rate-limit-remaining` absent or unparseable | Same per-endpoint marking, but the response also advances a per-account count: **3 consecutive header-less 88s with no successful response in between are treated as an account restriction**: the account is soft-deactivated for `BanCooldown` and the call returns an `IsBanned` error. Any success restarts the count, and so does the bench itself. |
| Restricted | 88, `x-rate-limit-remaining` > 0 | Soft-deactivate for `BanCooldown`. |
| Suspended | 64 | Permanent removal from the pool. |
| Locked | 326 | With a `CaptchaSolver`, one relogin is attempted; otherwise, or on failure, soft-deactivate for `BanCooldown`. There is no dedicated unlock flow, so a locked account usually needs attention in a browser. |
| Blocked / not authorized | 161, 179, 219 | Soft-deactivate for `AuthCooldown`. |
| Internal error | 131 | Treated as success if the body still carries `data`; otherwise retried. |
| Caller input rejected | 400 + GraphQL code 214 | `ErrBadRequest` — returned immediately: X rejected the request's own input (bad variable, cursor or handle), so another account would answer identically. No retry, no rotation, no account charge, no endpoint-fault bookkeeping. |
| Errors-only 2xx, deterministic miss | `errors[]` only + a miss marker | `ErrNotFound` — returned immediately: the query ran and the resource is absent (see the miss markers under `ErrNotFound` below). No retry, no rotation, no guest fallback, no relogin, no account charge. |
| Errors-only 2xx, unrecognised | `errors[]` only, no known class | `ErrUnexpectedResponse` — returned immediately: the query ran but every error entry matches no class the library knows. No retry, no rotation, no guest fallback, no relogin, no account charge, no endpoint-fault bookkeeping. |
| 2xx with no timeline | `GetUserTweets` answered with data that carries no timeline object (`{"data":{"user":{}}}`) | `ErrUnexpectedResponse` — terminal like the row above; the call already succeeded at the transport level, so nothing is retried or charged. A timeline object with zero entries is a valid empty profile and returns an empty list. |
| Other non-200 | — | Counts against the account's health; past the threshold the account is soft-deactivated with a growing backoff (`NonResponsiveCooldown`, doubling, capped at 30 minutes). |
| Transport failure | — | For an account with its own `Proxy`, a proxy-looking error (including a proxy CONNECT 407) is charged to the proxy: exponential backoff (`ProxyBackoffInitial` to `ProxyBackoffMax`) and a `proxy.down` alert, not an account failure. Any other transport error counts against the account. |

Observability note: each header-less 88 is still recorded as `rate_limited` in `request.duration` (and in `MetricsHook` the first two of the run are rate-limited calls) — that is the per-response truth. The escalation itself is signalled by the `account banned (repeated header-less code 88)` log warning, a non-rate-limited `MetricsHook` outcome on the third call, and the pool's `alerts.item_deactivated` alert — not by a `banned` outcome on the instrument.

Errors returned by the client wrap `*twitter.APIError` (`Status`, `Codes`, `Message`, `Err`). Test them with `errors.As` or these predicates, which unwrap for you:

| Predicate | True for |
|-----------|----------|
| `IsRateLimited` | HTTP 429, or code 88 with `x-rate-limit-remaining` 0 or absent (a header-less 88 stays `IsRateLimited` below the 3-in-a-row escalation) |
| `IsBanned` | code 88 with `x-rate-limit-remaining` > 0, or the third consecutive header-less 88 |
| `IsSuspended` | code 64 |
| `IsLocked` | code 326 |
| `IsCSRF` | code 353 |
| `IsAuthExpired` | code 32 |
| `IsBlocked` | code 161 |
| `IsNotAuthorized` | codes 179, 219 |
| `IsInternal` | code 131 |
| `IsForbidden` | HTTP 403 with no recognized code |

`twitter.ErrNotFound` marks deterministic misses (deleted tweet, suspended or unavailable user): the account worked, the resource is not there. Besides the parser-level detections, an errors-only 2xx maps to `ErrNotFound` when any `errors[]` entry carries a miss marker: code `34`, `50`, `63` or `144` (top level or inside `extensions`), `name`/`kind` `NotFoundError` (top level or inside `extensions`), or a message starting with `_Missing:`. Test it with `errors.Is`.

`twitter.ErrUnexpectedResponse` marks any other errors-only 2xx: X answered the committed request with errors the library does not recognise. It is terminal — no retry, no rotation, no guest fallback, no relogin, no account charge — and is NOT an endpoint fault: the `errors[]` envelope proves the request shape executed. Match it with `errors.Is`. The same sentinel is returned by `GetUserTweets` when the response data carries no timeline at all, so a lost timeline is never read as "no new tweets".

`twitter.ErrBadRequest` marks deterministic caller-input rejections: a GraphQL 400 whose error entries carry code `214` (top-level or inside `extensions`). A bad cursor, handle or variable is the caller's own error — it is never retried, never rotated to another account, never charged to account health, and never counted as an endpoint fault. A GraphQL 400 WITHOUT code 214 keeps the endpoint-fault classification: a stale `features` set answers `kind:"Validation"`/`name:"BadRequestError"` too (code 336), so the kind/name markers alone do not separate caller errors from stale request shapes — only the numeric code does.

`twitter.ErrEndpoint` (HTTP 404, or GraphQL-shaped 400 without code 214) and `twitter.ErrOverloaded` (HTTP 5xx, `LoadShed`) are endpoint-side faults: the committed request shape or X itself is at fault, so the serving account is never charged and the call rotates. Test all sentinels with `errors.Is`.

## Relogin circuit breaker

Relogin logs in on a separate candidate and replaces the account's credentials only when the login succeeds, so a failed relogin leaves them intact. To keep a login outage from turning into a stream of login attempts, `NewClient` installs a per-account breaker by default: after 3 consecutive failed relogins for an account, further relogins for it are refused for 30 minutes. A successful relogin resets the count. A refused relogin surfaces as a `relogin blocked` error on that attempt.

To opt out and always relogin, install an always-allow gate after `NewClient`:

```go
client.SetAutoReloginGate(twitter.AutoReloginGateFunc(
    func(context.Context, string) (bool, string) { return true, "" }))
```

Any type implementing `AutoReloginGate` (`Allowed(ctx, username) (bool, reason)`) can replace the breaker; a nil gate also allows everything. A custom gate that does not implement `RecordFailure`/`RecordSuccess` receives no outcome feedback.
