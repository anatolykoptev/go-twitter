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
- **Session Persistence** — JSON file cache with TTL
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

The `social` package is a thin client for an external account-leasing service. `SearchWithSocial`, `SearchTweetsWithSocial`, `SearchUsersWithSocial`, `GetUserWithSocial`, `GetUserTweetsWithSocial` and `GetTweetConversationWithSocial` in the root package lease an account, run the call, retry on other accounts, and report the outcome.

## Error Handling

Every request makes up to 3 attempts, rotating accounts on pool-based calls. Recovery per error class:

| Error | Code | Action |
|-------|------|--------|
| CSRF mismatch | 353 | Adopt the ct0 from the error response's `set-cookie` and retry. If x.com offers none, or the retry fails, relogin (subject to the breaker) and retry once. A random ct0 is never generated. |
| Auth expired | 32 | Relogin (subject to the breaker) and retry; on failure soft-deactivate the account for `AuthCooldown`. |
| Rate limited | 429 | Mark the endpoint rate-limited for that account until `x-rate-limit-reset` (15 minutes if absent) and try another account. |
| Banned | 88 | Soft-deactivate for `BanCooldown`. |
| Suspended | 64 | Permanent removal from the pool. |
| Locked | 326 | With a `CaptchaSolver`, relogin to solve the challenge and retry; otherwise, or on failure, soft-deactivate for `BanCooldown`. |
| Blocked / not authorized | 161, 179, 219 | Soft-deactivate for `AuthCooldown`. |
| Internal error | 131 | Treated as success if the body still carries `data`; otherwise retried. |
| Other non-200 | — | Counts against the account's health; past the threshold the account is soft-deactivated with a growing backoff (`NonResponsiveCooldown`, doubling, capped at 30 minutes). |
| Transport failure | — | For an account with its own `Proxy`, a proxy-looking error (including a proxy CONNECT 407) is charged to the proxy: exponential backoff (`ProxyBackoffInitial` to `ProxyBackoffMax`) and a `proxy.down` alert, not an account failure. Any other transport error counts against the account. |

Errors returned by the client wrap `*twitter.APIError` (`Status`, `Codes`, `Message`, `Err`). Test them with `errors.As` or these predicates, which unwrap for you:

| Predicate | True for |
|-----------|----------|
| `IsRateLimited` | HTTP 429 |
| `IsBanned` | code 88 |
| `IsSuspended` | code 64 |
| `IsLocked` | code 326 |
| `IsCSRF` | code 353 |
| `IsAuthExpired` | code 32 |
| `IsBlocked` | code 161 |
| `IsNotAuthorized` | codes 179, 219 |
| `IsInternal` | code 131 |
| `IsForbidden` | HTTP 403 with no recognized code |

`twitter.ErrNotFound` marks deterministic misses (deleted tweet, suspended or unavailable user): the account worked, the resource is not there. Test it with `errors.Is`.

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

Endpoints without a seed use `ClientConfig.RateLimit` (default 50 per 15 minutes). On every response from a pool request or an account-pinned POST that carries a positive `x-rate-limit-limit`, the account's cap for that endpoint is updated to it, so the limiter follows what x.com currently grants. Media uploads only record 429s. The pool skips accounts whose limiter denies the endpoint.

On top of that, each account is paced individually on pool requests: a random delay of `AccountPaceMin` to `AccountPaceMin + AccountPaceRandom` (default 0.8s to 2.0s) between consecutive requests by the same account. Pacing is keyed by account, so a busy caller does not delay callers using other accounts. Account-pinned POSTs and media uploads are not paced. Disable pacing with `AccountPaceDisabled`.

## Anti-Detection

- **xtid** — `x-client-transaction-id` generated from x.com page animation keys, refreshed every 30 minutes. If initialization fails, the header is omitted and a `xtid.init_failed` alert is sent to `PoolAlertHook`.
- **xpff** — `x-xp-forwarded-for` generated from the session's `guest_id` and user agent.
- **CT0** — never rotated on a timer or replaced with a client-generated value on an established session; x.com validates it server-side, so doing so causes CSRF 353 errors. The client adopts the ct0 from each successful response's `set-cookie`, persists it to the session file, and on a 353 adopts the server-offered ct0 before falling back to relogin. A generated ct0 is used only at login, when x.com sets none.
- **TLS** — browser-grade JA3 fingerprints via go-stealth, with a per-account browser profile (`AssignBrowserProfile`).
- **Headers** — fixed header ordering and client hints derived from the account's user agent.
- **Jitter** — randomized delay before every request.

## Configuration

`ClientConfig` fields (zero values take the defaults shown):

| Field | Default | Purpose |
|-------|---------|---------|
| `Accounts` | — | Accounts for the pool. |
| `DefaultProxy` | none | Proxy for the shared client and for accounts without their own `Proxy`. |
| `SessionTTL` | 24h | How long a saved session is reused. |
| `SessionDir` | `~/.go-twitter/sessions` | Where sessions are persisted. |
| `AuthCooldown` | 1h | Soft-deactivation after auth errors. |
| `BanCooldown` | 6h | Soft-deactivation after ban or lock. |
| `NonResponsiveCooldown` | 5m | Base backoff for repeated non-200 failures (doubles, capped at 30m, ±30% jitter). |
| `CaptchaSolver` | none | Solver for login challenges and locked accounts (`captcha.NewCapsolver`). |
| `RateLimit` | 50 per 15m | Cap for endpoints without a measured seed. |
| `AccountPaceMin` / `AccountPaceRandom` | 800ms / 1.2s | Per-account request spacing. |
| `AccountPaceDisabled` | false | Turn per-account pacing off. |
| `ProxyBackoffInitial` / `ProxyBackoffMax` | 30s / 30m | Backoff for failing proxies. |
| `OpenAccountCount` | 0 | Anonymous guest sessions to create at startup. |
| `DisableGuestFallback` | false | Refuse the guest-token fallback on endpoints that allow it. |
| `PoolAlertHook` | log a warning | `func(topic string, payload any)` called on pool events. This repo emits `proxy.down` (user, masked proxy, consecutive failures, backoff) and `xtid.init_failed`; the go-stealth pool emits its own topics, such as account deactivation. |

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

x.com rotates GraphQL queryIDs and feature-flag defaults inside its JS bundle; a stale ID or flag set returns HTTP 400. `cmd/gql-sync` extracts the current values from x.com's live bundles and regenerates `queryids_gen.go` and `features_gen.go`. At runtime the effective queryID for an operation is resolved as environment override (`TWITTER_QID_*`) over generated over committed literal. Operations that are not reachable from the public bundles (the authenticated read cluster and the like/retweet mutations) are maintained by hand, with the env override as the hotfix path.

```bash
go run ./cmd/gql-sync -proxy "$PROXY_URL" -fail-on-empty
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
