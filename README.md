# go-twitter

[![Go 1.26.3+](https://img.shields.io/badge/Go-1.26.3+-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Twitter/X scraping library for Go — multi-account pool, GraphQL API, anti-ban, CAPTCHA solving, TOTP 2FA, and session persistence.

Built on [go-stealth](https://github.com/anatolykoptev/go-stealth) for TLS fingerprinting, proxy rotation, and rate limiting.

## Features

- **Account Pool** — round-robin rotation with per-account health tracking, per-endpoint rate limits and per-account pacing
- **GraphQL API** — profiles, timelines, followers, search, conversations, lists, communities, bookmarks, home timelines; likes, retweets, tweets, replies; DMs; media upload
- **Anti-Ban** — TLS fingerprinting, header ordering, client hints, `x-client-transaction-id` (xtid), `x-xp-forwarded-for` (xpff)
- **Auth** — multi-step login flow with password, TOTP 2FA, CAPTCHA (Capsolver)
- **Error Recovery** — CSRF recovery from the server's ct0, session refresh behind a relogin circuit breaker, ban cooldown, proxy-failure backoff
- **Typed Errors** — `*APIError` plus `Is*` predicates
- **Session Persistence** — pluggable `SessionStore`; JSON file cache with TTL by default
- **Proxy Support** — per-account proxy, exponential backoff on proxy failures

## Install

```bash
go get github.com/anatolykoptev/go-twitter
```

Requires Go 1.26.3 or newer (see [go.mod](go.mod)).

## Quick Start

```go
cfg := twitter.ClientConfig{
    Accounts: []*twitter.Account{
        {Username: "user1", Password: "pass1", TOTPSecret: "BASE32SECRET"},
        {Username: "user2", AuthToken: "existing_token", CT0: "existing_ct0"},
    },
    DefaultProxy:  "http://proxy:8080",
    CaptchaSolver: captcha.NewCapsolver(os.Getenv("CAPSOLVER_KEY")),
}

client, err := twitter.NewClient(cfg)

// Read (the pool picks an account)
user, _ := client.GetUserByScreenName(ctx, "elonmusk")
tweets, _ := client.GetUserTweets(ctx, user.ID, 50)
followers, _ := client.GetFollowers(ctx, user.ID, 100)

// Search
latest, _ := client.SearchTweets(ctx, "$BTC", twitter.SearchLatest, 20)
people, _ := client.SearchUsers(ctx, "go developer", 20)

// Write (account-specific)
tweetID, _ := client.PostWithAccount(ctx, "user1", "Hello Twitter!")
acc := client.AccountByUsername("user1")
_ = client.LikeTweet(ctx, acc, tweetID)
```

An account needs either `AuthToken` + `CT0`, a saved session, or `Username` + `Password` (plus `TOTPSecret` if 2FA is on). `ParseAccounts("user:pass[:auth_token[:ct0[:totp_secret]]],...")` builds the slice from a comma-separated string.

## API

All methods below are on `*twitter.Client`. Every one of them needs an authenticated account; there is no guest path for any exported method. Auth requirements are defined by `requiresAuth` in [request.go](request.go).

- **Pool** — the pool picks the account, rotates on failure, and waits up to 5 minutes for an account whose endpoint limit allows the call.
- **Given** — you pass the `*Account` that performs the call (account-pinned; no rotation).

### Read

| Method | Account | Description |
|--------|---------|-------------|
| `GetUserByScreenName(ctx, handle)` | Pool | Profile by handle. `ErrNotFound` for suspended or unavailable users. |
| `GetUserTweets(ctx, userID, count)` | Pool | A user's recent tweets. |
| `GetTweetByID(ctx, tweetID)` | Pool | One tweet (via TweetDetail). `ErrNotFound` if absent from the page. |
| `GetTweetConversation(ctx, tweetID)` | Pool | Focal tweet plus its conversation entries (replies and ancestors, in page order). |
| `GetFollowers(ctx, userID, max)` | Pool | Paginated follower list. |
| `GetFollowing(ctx, userID, max)` | Pool | Paginated following list. |
| `GetVerifiedFollowers(ctx, userID, max)` | Pool | Paginated blue-verified followers. |
| `GetRetweeters(ctx, tweetID, max)` | Pool | Users who retweeted a tweet. |
| `GetListTweets(ctx, listID, max)` | Pool | Latest tweets in a list. |
| `GetCommunityTweets(ctx, communityID, max)` | Pool | Latest tweets in a community. |
| `GetBookmarks(ctx, max)` | Pool | The pool account's bookmarks. |
| `GetHomeTimeline(ctx, count)` | Pool | Algorithmic "For you" timeline. |
| `GetHomeLatestTimeline(ctx, count)` | Pool | Reverse-chronological "Following" timeline. |

### Search

| Method | Account | Description |
|--------|---------|-------------|
| `SearchTweets(ctx, query, product, count)` | Pool | Tweet search on `SearchTop` or `SearchLatest`. Any other product is rejected before the request. |
| `SearchTimeline(ctx, query, count)` | Pool | Shorthand for `SearchTweets` on the Latest tab. |
| `SearchUsers(ctx, query, count)` | Pool | People search. |

`SearchPeople` and `SearchMedia` exist as `SearchProduct` values; `SearchUsers` uses `SearchPeople` internally and `SearchTweets` accepts only Top and Latest.

### Write

| Method | Account | Description |
|--------|---------|-------------|
| `CreateTweet(ctx, acc, text)` | Given | Post a tweet; returns its ID. |
| `PostWithAccount(ctx, username, text)` | By username | `CreateTweet` for the pool account with that username (must be active). |
| `ReplyTweet(ctx, acc, tweetID, text)` | Given | Reply to a tweet; returns the reply ID. |
| `LikeTweet(ctx, acc, tweetID)` | Given | Like a tweet. |
| `UnlikeTweet(ctx, acc, tweetID)` | Given | Remove a like. |
| `Retweet(ctx, acc, tweetID)` | Given | Retweet; returns the new retweet ID. |
| `Unretweet(ctx, acc, tweetID)` | Given | Remove a retweet; takes the original tweet's ID. |

### DM

| Method | Account | Description |
|--------|---------|-------------|
| `GetDMInbox(ctx)` | Pool | The pool account's DM conversations with the messages present in the initial inbox state (1.1 API). |
| `SendDM(ctx, acc, conversationID, text)` | Given | Send a text DM into an existing conversation; returns the message ID. `conversationID` is a conversation ID (1:1 form `<recipientID>-<selfID>`), not a user ID. |

### Media

| Method | Account | Description |
|--------|---------|-------------|
| `UploadMedia(ctx, acc, data, mimeType)` | Given | Chunked upload (INIT/APPEND/FINALIZE, polls async video/GIF processing); returns the media ID. |
| `UploadMediaFile(ctx, acc, path)` | Given | Reads a file, infers the MIME type, uploads it. |
| `CreateTweetWithMedia(ctx, acc, text, mediaIDs)` | Given | Post a tweet with uploaded media attached. |
| `PostWithMedia(ctx, username, text, paths...)` | By username | Upload the files and post them from the named account. |

### Pool and account helpers

| Method | Description |
|--------|-------------|
| `AccountByUsername(username)` | Look up a pool account (case-insensitive); nil if absent. |
| `HealthReport()` | Per-account totals, consecutive failures, and proxy backoff state. |
| `Pool()` | The underlying go-stealth account pool. |
| `ValidateAccount(ctx, acc)` | Check an account's credentials against the account settings endpoint. |
| `SetAutoReloginGate(gate)` | Replace the relogin circuit breaker (see below). |

The `social` package is a thin client for an external account-leasing service. `SearchWithSocial`, `SearchTweetsWithSocial`, `SearchUsersWithSocial`, `GetUserWithSocial`, `GetUserTweetsWithSocial` and `GetTweetConversationWithSocial` in the root package lease an account, run the call, retry on other accounts, and report the outcome. The repeated-header-less-88 escalation does not fire on this path: each attempt builds a fresh `Account`, so the count never reaches the threshold — a leased account that keeps answering 88 is reported to the credential service as `auth_error`, not `banned`.

A caller-side cancellation or deadline (`context.Canceled`, `context.DeadlineExceeded`, or the caller ctx ending mid-request) is NOT the leased account's fault: it reports nothing to go-social (the vocabulary has no neutral status) and stops leasing immediately. Reports are sent on a detached, bounded context, so a `success` verdict still lands when the caller's ctx died before the response arrived.

## Error Handling

Every request makes up to 3 attempts, rotating accounts on pool-based calls. Recovery per error class:

| Error | Code | Action |
|-------|------|--------|
| CSRF mismatch | 353 | Adopt the ct0 from the error response's `set-cookie` and retry. If x.com offers none, or the retry fails, relogin (subject to the breaker) and retry once. A random ct0 is never generated. |
| Auth expired | 32 | Relogin (subject to the breaker) and retry; on failure soft-deactivate the account for `AuthCooldown`. |
| Rate limited | 429 | Mark the endpoint rate-limited for that account until `x-rate-limit-reset` (15 minutes if absent) and try another account. A 200 whose `x-rate-limit-remaining` is already spent holds the endpoint the same way, before any 429 (see Rate Limiting). |
| Rate limited (in a 200 body) | 88, `x-rate-limit-remaining` 0 | Same as 429: mark only that endpoint rate-limited for that account until `x-rate-limit-reset` (15 minutes if absent; a reset in the past is raised to 1 second from now). The account stays active. |
| Rate limited, header-less (in a 200 body) | 88, `x-rate-limit-remaining` absent or unparseable | Same per-endpoint marking, but the response also advances a per-account count: **3 consecutive header-less 88s with no successful response in between are a ban signature** — the account is soft-deactivated for `BanCooldown` and the call returns an `IsBanned` error. Any success restarts the count, and so does the bench itself. |
| Banned | 88, `x-rate-limit-remaining` > 0 | Soft-deactivate for `BanCooldown`. |
| Suspended | 64 | Permanent removal from the pool. |
| Locked | 326 | With a `CaptchaSolver`, relogin to solve the challenge and retry; otherwise, or on failure, soft-deactivate for `BanCooldown`. |
| Blocked / not authorized | 161, 179, 219 | Soft-deactivate for `AuthCooldown`. |
| Internal error | 131 | Treated as success if the body still carries `data`; otherwise retried. |
| Caller input rejected | 400 + GraphQL code 214 | `ErrBadRequest` — returned immediately: X rejected the request's own input (bad variable, cursor or handle), so another account would answer identically. No retry, no rotation, no account charge, no endpoint-fault bookkeeping. |
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

`twitter.ErrNotFound` marks deterministic misses (deleted tweet, suspended or unavailable user): the account worked, the resource is not there. Test it with `errors.Is`.

`twitter.ErrBadRequest` marks deterministic caller-input rejections: a GraphQL 400 whose error entries carry code `214` (top-level or inside `extensions`). A bad cursor, handle or variable is the caller's own error — it is never retried, never rotated to another account, never charged to account health, and never counted as an endpoint fault. A GraphQL 400 WITHOUT code 214 keeps the endpoint-fault classification: a stale `features` set answers `kind:"Validation"`/`name:"BadRequestError"` too (code 336), so the kind/name markers alone do not separate caller errors from stale request shapes — only the numeric code does.

`twitter.ErrEndpoint` (HTTP 404, or GraphQL-shaped 400 without code 214) and `twitter.ErrOverloaded` (HTTP 5xx, `LoadShed`) are endpoint-side faults: the committed request shape or X itself is at fault, so the serving account is never charged and the call rotates. Test all sentinels with `errors.Is`.

### Relogin circuit breaker

Relogin clears an account's credentials before logging in again, so a login outage can destroy a still-valid session. To prevent that, `NewClient` installs a per-account breaker by default: after 3 consecutive failed relogins for an account, further relogins for it are refused for 30 minutes, and the account's credentials are left intact. A successful relogin resets the count. A refused relogin surfaces as a `relogin blocked` error on that attempt.

To opt out and always relogin, install an always-allow gate after `NewClient`:

```go
client.SetAutoReloginGate(twitter.AutoReloginGateFunc(
    func(context.Context, string) (bool, string) { return true, "" }))
```

Any type implementing `AutoReloginGate` (`Allowed(ctx, username) (bool, reason)`) can replace the breaker; a nil gate also allows everything. A custom gate that does not implement `RecordFailure`/`RecordSuccess` receives no outcome feedback.

## Rate Limiting

Limits are per account and per endpoint (operation name such as `Followers`), in 15-minute windows. Each account's limiter is seeded with caps measured from x.com's `x-rate-limit-limit` header:

| Endpoint | Seed (requests / 15 min / account) |
|----------|------------------------------------|
| `UserByScreenName` | 150 |
| `Followers` | 187 |
| `Following` | 187 |
| `Retweeters` | 500 |
| `UserTweets` | 50 |
| `SearchTimeline` | 187 |
| `HomeLatestTimeline` | 500 |
| `DeviceFollow` | 180 |

The table is exported as `EndpointLimits()`, which returns a copy — mutating the result changes nothing. Only measured operations are listed; `RateLimitHeadroom` (2) exports the exhaustion margin the header authority applies, so a consumer can compute an account's effective budget without duplicating either constant.

Endpoints without a seed use `ClientConfig.RateLimit` (default 50 per 15 minutes). On every response from a pool request, an account-pinned POST or a media upload that carries a positive `x-rate-limit-limit`, the account's cap for that endpoint is updated to it, so the limiter follows what x.com currently grants. The pool skips accounts whose limiter denies the endpoint.

**x.com's headers are the authority.** Every response (200 and non-200) that carries both `x-rate-limit-remaining` and `x-rate-limit-reset` updates that account's window for the endpoint. An older response never raises `remaining` inside one window (windows are ordered by their reset timestamp; within a window `remaining` only falls). A response whose `remaining` is at most `RateLimitHeadroom` (2, spare slots for requests already in flight) persists a hold until the reset — that is X's own exhaustion, and nothing client-side releases it early; a real 429 or code 88 marks the endpoint the same way. On top of that the account's budget share gates at read time: the endpoint is held once the window's `remaining` estimate reaches `max(RateLimitHeadroom, limit − max(1, limit*pct/100))` — the complement of the scaled local-counter cap, but never below the in-flight headroom, which applies at every share including the full budget. The budget floor is derived, never persisted — a runtime `SetBudgetPct` that raises the share releases a budget-only hold on the very next pick, while reported exhaustion stands until the reset. In both cases the call that reported the window still succeeds and the next pick rotates to another account, so a 429 that the headers announced never happens. While an endpoint's window is live (before its reset) the local request counter is ignored; the counter decides before the first header is seen (cold start), when responses carry no headers, and again once a window has expired without a renewing response. Writes and media uploads record the window but do not rotate. Observed values are exported as the `twitter.client.ratelimit.*` metrics (see `docs/metrics.md`).

**Per-account spend share.** `Account.SetBudgetPct(pct)` restricts the account to `pct` percent of each endpoint's window (1-100; every out-of-range value — 0, negative, above 100 — means 100, the whole window, which is also the never-set default). Both gates enforce the same spend, `max(1, limit*pct/100)` requests per window: the local-counter cap is scaled to it, and under header authority the endpoint is held once `remaining` reaches `limit` minus that spend, floored at `RateLimitHeadroom` at every share. A share of zero cannot be expressed — benching an account entirely is `HoldAccount`'s job. `Account.BudgetPct()` reads it back. It is safe to call at runtime on a live pooled account — the next pick sees the new share, including the release of a budget-only hold.

**Account hold.** `Client.HoldAccount(username, until)` benches a pooled account for every endpoint until the given time — the pool skips it and any waiting call wakes when the hold ends. Re-holding is max-wins: a later call with an earlier `until` never shortens an existing hold; `Client.ReleaseAccount(username)` lifts a hold early. The hold gates only the pool pick — account-pinned writes (`CreateTweet`, `FollowUser`, `UploadMedia`) do not consult it. It is not a failure: no health penalty, no failure counters, no relogin. Returns false when no pool account matches; when every account is held the caller gets the same error a fully-spent pool returns: `pool exhausted for <endpoint> (requires auth): all N items are unavailable`.

**Request observer.** `ClientConfig.OnRequest func(RequestEvent)`, when set, is called once per HTTP attempt a pooled account served — after the response is classified, on success and on every error path — at the same observation point that feeds `twitter.client.request.duration`. `RequestEvent` carries the username, endpoint, HTTP status, the shared-classifier `Outcome` (the `twitter.outcome` values in `docs/metrics.md`), and a `RequestRateLimit` mirroring the response's `x-rate-limit-*` headers (`Limit` 0, `Remaining` -1 and a zero `Reset` mean unknown). Media-upload attempts emit under the `UploadMedia` endpoint label; guest-token fallback attempts carry no account and emit nothing. The hook runs synchronously on the request path and without a recover — it must not block and must not panic.

The `*WithSocial` helpers lease a fresh single-account client per attempt, so that state would die with the lease. They therefore pass the operation to go-social as `?endpoint=<operation>` on acquire and send the last observed window back with the usage report as `rate_limit` (`endpoint`, `limit`, `remaining`, `reset`); see `social.Client.AcquireAccountFor` and `ReportUsageWithRateLimit`. A go-social that predates these fields ignores them.

On top of that, each account is paced individually on pool requests: a random delay of `AccountPaceMin` to `AccountPaceMin + AccountPaceRandom` (default 0.8s to 2.0s) between consecutive requests by the same account. Pacing is keyed by account, so a busy caller does not delay callers using other accounts. Account-pinned POSTs and media uploads are not paced. Disable pacing with `AccountPaceDisabled`.

Independently of pacing, `MaxInFlightPerAccount` (default 2) bounds how many requests may be in flight on one account at once: the pool skips an account at the cap, and when every eligible account is full the call waits for a slot on the same ctx-bounded machinery as endpoint-limit waits (`pool.wait` metric); the account-pinned paths wait for their own account's slot. A negative value disables the cap. Live in-flight counts are exported as the `twitter.client.pool.inflight` gauge (see `docs/metrics.md`). Caveat for `*WithSocial` callers: each lease builds a fresh single-account client, so the cap cannot span concurrent leases of the same underlying account — cross-lease concurrency control belongs to go-social.

## Anti-Detection

- **xtid** — `x-client-transaction-id` generated from x.com page animation keys, refreshed every 30 minutes. Initialised lazily on the first request carrying an account's cookies (logged-out x.com serves a shell without the bundle). If initialization fails, the header is omitted, retries back off from 60 s to 15 min, and one `xtid.init_failed` alert per failure episode is sent to `PoolAlertHook`.
- **xpff** — `x-xp-forwarded-for` generated from the session's `guest_id` and user agent.
- **CT0** — never rotated on a timer or replaced with a client-generated value on an established session; x.com validates it server-side, so doing so causes CSRF 353 errors. The client adopts the ct0 from each successful response's `set-cookie`, persists it to the session file, and on a 353 adopts the server-offered ct0 before falling back to relogin. A generated ct0 is used only at login, when x.com sets none.
- **TLS** — browser-grade JA3 fingerprints via go-stealth, with a per-account browser profile (`AssignBrowserProfile`).
- **Headers** — fixed header ordering and client hints derived from the account's user agent.
- **Jitter** — no pre-request delay by default (the per-account pacer still spaces one account's requests); opt in with `WithRequestJitter(min, max)`. Writes and media uploads keep their own jitter.

**Pre-request jitter is off by default.** Earlier versions slept a random 0.5-2.5 s before every
pooled request, which made up most of a read's latency (median 1.49 s of a 1.99 s call). Account
rotation and quotas are expected to be handled by the service that leases accounts, and the
per-account pacer (`WithAccountPace`, default 0.8 s + random) still spaces consecutive requests of one
account. To restore the old delay: `twitter.NewClient(cfg, twitter.WithRequestJitter(500*time.Millisecond, 2500*time.Millisecond))`.
Writes and media uploads keep their own jitter either way.
The `*WithSocial` helpers build a client per call, so they share one pacer per account across calls
(within one process, and per pace config: consecutive calls that lease the same account with the same pace are spaced by it; other processes and other pace settings do not share state) and take the same options as a
trailing argument, e.g. `twitter.GetUserWithSocial(ctx, sc, "handle", twitter.WithRequestJitter(500*time.Millisecond, 2500*time.Millisecond))`.

## Configuration

`ClientConfig` fields (zero values take the defaults shown):

| Field | Default | Purpose |
|-------|---------|---------|
| `Accounts` | — | Accounts for the pool. |
| `DefaultProxy` | none | Proxy for the shared client and for accounts without their own `Proxy`. |
| `Account.Proxy` | none | Per-account proxy. A malformed one disables only that account (`account.proxy_client_failed`); `NewClient` fails only when no account is usable, so a single-account client still fails. |
| `SessionTTL` | 24h | How long the default file store reuses a saved session. Ignored by a custom `SessionStore`. |
| `SessionDir` | `~/.go-twitter/sessions` | Where the default file store persists sessions. Ignored when `SessionStore` is set. |
| `SessionStore` | file store | Where sessions are persisted; see [Session storage](#session-storage). |
| `AuthCooldown` | 1h | Soft-deactivation after auth errors. |
| `BanCooldown` | 6h | Soft-deactivation after ban or lock. |
| `NonResponsiveCooldown` | 5m | Base backoff for repeated non-200 failures (doubles, capped at 30m, ±30% jitter). |
| `CaptchaSolver` | none | Solver for login challenges and locked accounts (`captcha.NewCapsolver`). |
| `RateLimit` | 50 per 15m | Cap for endpoints without a measured seed. |
| `MaxInFlightPerAccount` | 2 | Max concurrent requests per account. Pooled picks skip accounts at the cap and wait (bounded by the caller's ctx, on the same `pool.wait` machinery as endpoint limits) when every eligible account is full; the account-pinned POST and media paths wait for their account's slot. A negative value disables the cap. See [Rate Limiting](#rate-limiting). |
| `AccountPaceMin` / `AccountPaceRandom` | 800ms / 1.2s | Per-account request spacing. |
| `AccountPaceDisabled` | false | Turn per-account pacing off. |
| `ProxyBackoffInitial` / `ProxyBackoffMax` | 30s / 30m | Backoff for failing proxies. |
| `OpenAccountCount` | 0 | Anonymous guest sessions to create at startup. |
| `DisableGuestFallback` | false | Refuse the guest-token fallback on endpoints that allow it. |
| `PoolAlertHook` | log a warning | `func(topic string, payload any)` called on pool events. This repo emits `proxy.down` (user, masked proxy, consecutive failures, backoff) and `xtid.init_failed`; the go-stealth pool emits its own topics, such as account deactivation. `account.proxy_client_failed` (a per-account proxy client failed to build; payload keys `user`, `proxy` (masked label), `error` (fixed text)). |
| `OnRequest` | nil (no-op) | `func(RequestEvent)` called once per pooled-account HTTP attempt after classification — the request observer; see [Rate Limiting](#rate-limiting). Must not block. |

## Session storage

Sessions (`auth_token` + `ct0`) are loaded at startup, re-saved whenever x.com rotates `ct0`, and deleted on relogin. By default they live in `<SessionDir>/<username>.json` (`FileSessionStore`, atomic writes, 0600). Set `ClientConfig.SessionStore` to keep them elsewhere, for example in the database of your credential service:

```go
type dbStore struct{ db *sql.DB }

func (s dbStore) Load(ctx context.Context, user string) (twitter.Session, error) {
    var ss twitter.Session
    err := s.db.QueryRowContext(ctx, `SELECT auth_token, ct0, saved_at FROM tw_sessions WHERE username=$1`, user).
        Scan(&ss.AuthToken, &ss.CT0, &ss.SavedAt)
    if errors.Is(err, sql.ErrNoRows) {
        return twitter.Session{}, twitter.ErrSessionNotFound
    }
    return ss, err
}
func (s dbStore) Save(ctx context.Context, user string, ss twitter.Session) error {
    _, err := s.db.ExecContext(ctx, `INSERT INTO tw_sessions (username, auth_token, ct0, saved_at) VALUES ($1,$2,$3,$4)
        ON CONFLICT (username) DO UPDATE SET auth_token=$2, ct0=$3, saved_at=$4`, user, ss.AuthToken, ss.CT0, ss.SavedAt)
    return err
}
func (s dbStore) Delete(ctx context.Context, user string) error {
    _, err := s.db.ExecContext(ctx, `DELETE FROM tw_sessions WHERE username=$1`, user)
    return err
}

c, err := twitter.NewClient(twitter.ClientConfig{Accounts: accs, SessionStore: dbStore{db}})
```

Contract: `Load` returns `ErrSessionNotFound` on a miss. Expiry is the store's job: the client applies no TTL, and `SessionTTL` only configures the default file store. Every call gets a context bounded to 5s, and a store error is logged (username only) but never fails an API request. Stores must be safe for concurrent use. Only credentials leased by the `*WithSocial` helpers bypass the store; static-config accounts still load the stored session first. A `Load` error other than `ErrSessionNotFound` is not treated as a miss: provided credentials are used as-is, and an account with none fails instead of logging in with its password.

## Types

```go
type TwitterUser struct {
    ID, Handle, DisplayName, Bio string
    Followers, Following, TweetCount, ListedCount int
    CreatedAt                      time.Time
    IsVerified, HasAvatar, HasBio  bool
}

type Tweet struct {
    ID, AuthorID, AuthorHandle, AuthorName, Text string
    CreatedAt                                    time.Time
    Views, Likes, Retweets, Quotes, ReplyCount   int
    TokenMentions                                []string // extracted $TICKER mentions
}

type DMConversation struct {
    ConversationID string
    Participants   []string // user IDs
    Messages       []DMMessage
}

type DMMessage struct {
    ID, ConversationID, SenderID, Text string
    CreatedAt                          time.Time
}
```

## GraphQL ID maintenance (gql-sync)

x.com rotates GraphQL queryIDs and feature-flag defaults inside its JS bundle; a stale ID or flag set returns HTTP 400. `cmd/gql-sync` extracts the current values from x.com's live bundles and regenerates `queryids_gen.go` and `features_gen.go`. At runtime the effective queryID for an operation is resolved as environment override (`TWITTER_QID_*`) over runtime-refreshed over generated over committed literal.

**Runtime refresh.** A process that holds an authenticated account also re-reads the IDs from x.com's own bundle (the authenticated page it already fetches for `x-client-transaction-id`), so an X rotation heals without a release. A refresh runs in the background, on the first authenticated request, after 6 hours, and after 3 endpoint faults (HTTP 404 / GraphQL-shaped 400) on one operation within 2 minutes; refreshes are at least 15 minutes apart, and consecutive failed or rejected refreshes back off exponentially (15 min, 30 min, ... capped at 24 h; a success resets it; the fault trigger obeys it too). A later refresh overlays the earlier one: ids it did not find are carried forward, never reverted. The whole extraction is validated (every ID must match `^[A-Za-z0-9_-]{1,64}$`, at least 40% of the known operations must be present, a malformed ID rejects it all) and swapped in atomically; on any failure the last good IDs stay and one `queryid.refresh_failed` pool alert fires per ok-to-failed transition (the hook is process-wide: the last `NewClient` that sets `PoolAlertHook` owns it). The request that hit the fault is not retried — the next call uses the new ID. Feature flags are only added to, never changed or removed. `Endpoints[op]` keeps the compiled value; the layered value is what requests use (`EndpointURL`). Set `TWITTER_QID_REFRESH=off` to disable. Metrics: [docs/metrics.md](docs/metrics.md). Operations that are not reachable from the public bundles (the authenticated read cluster and the like/retweet mutations) are maintained by hand, with the env override as the hotfix path.

```bash
go run ./cmd/gql-sync -proxy "$PROXY_URL" -fail-on-empty
# logged-out x.com serves a shell without the bundle; pass an account's cookies
# via the environment (never as a flag; sent to x.com and twitter.com hosts only, over https, never logged):
GQL_SYNC_COOKIES='auth_token=...; ct0=...' go run ./cmd/gql-sync -fail-on-empty
```

Flags: `-out` (repo root, default `.`), `-proxy`, `-fixtures` (read from a directory instead of the network), `-date`, `-fail-on-empty` (exit non-zero instead of leaving the files untouched when nothing is extracted).

## CI

**gql-sync drift check** (`.github/workflows/gql-sync-drift.yml`) — runs weekly
(Mon 06:00 UTC, plus manual `workflow_dispatch`). It runs gql-sync against x.com's **live** JS
bundle and, if the generated files changed, opens a PR for human review (it never auto-merges
generated values — a wrong queryID/flag default is an HTTP 400 on every call).

Requires repo secret **`TWITTER_PROXY`**: a proxy URL that can reach x.com from
GitHub's runner IPs (bare datacenter IPs are blocked). The job runs with
`-fail-on-empty`, so a missing proxy or a bundle break exits non-zero and fails
closed instead of overwriting the committed IDs with nothing.

**preflight** (`.github/workflows/preflight.yml`) — `make preflight` (gofmt, vet, build, test) on every PR and push to `main`.

## License

MIT
