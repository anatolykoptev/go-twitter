# API reference

All methods below are on `*twitter.Client`. Every one of them needs an authenticated account; there is no guest path for any exported method. Auth requirements are defined by `requiresAuth` in [request.go](../request.go).

- **Pool** — the pool picks the account, rotates on failure, and waits up to 5 minutes for an account whose endpoint limit allows the call.
- **Given** — you pass the `*Account` that performs the call (account-pinned; no rotation).

## Read

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
| `GetDeviceNotifications(ctx, count)` | Pool | Merged tweets from every author the account has notifications on for. Reads `GetDeviceFollowTweets` and falls back to `GetTweetNotifications` when that source fails. An empty answer is a normal quiet poll and does not trigger the fallback. |
| `GetDeviceFollowTweets(ctx, count)` | Pool | The same feed through the REST endpoint only. |
| `GetTweetNotifications(ctx, count)` | Pool | The same feed through GraphQL only. |
| `FriendshipState(ctx, acc, userID)` | Given | Whether `acc` follows `userID` and has notifications on. |

**Pagination.** `GetFollowers`, `GetFollowing`, `GetVerifiedFollowers`, `GetRetweeters`, `GetListTweets`, `GetCommunityTweets` and `GetBookmarks` page by cursor until the first of: `max` items collected; no cursor on the page; a page with no items (X can end a list with a non-empty cursor and nothing behind it); X's end-of-timeline cursor (`0|...` or a bare `0`); a cursor already followed in the call; or 5 consecutive pages that added no new item. Items are deduplicated by id within a call, so a server repeating items under fresh cursors is caught by the last rule; a list whose pages shrink but keep adding items is followed to `max`. Every one of these is a normal end: the call returns what it collected with a nil error, and logs a warning when `terminal_cursor` or `page_cap` cut it short of `max`. A page of only suspended or unavailable accounts still counts as a page with items. Users inside timeline modules are read like plain entries. The result may exceed `max` by up to one page. `ctx` ending or a request failing returns the items so far together with the error. Pages per call and the stop reason are recorded as `twitter.client.pagination.pages` and `twitter.client.pagination.stops` ([metrics](metrics.md)).

## Search

| Method | Account | Description |
|--------|---------|-------------|
| `SearchTweets(ctx, query, product, count)` | Pool | Tweet search on `SearchTop` or `SearchLatest`. Any other product is rejected before the request. |
| `SearchTimeline(ctx, query, count)` | Pool | Shorthand for `SearchTweets` on the Latest tab. |
| `SearchUsers(ctx, query, count)` | Pool | People search. |

`SearchPeople` and `SearchMedia` exist as `SearchProduct` values; `SearchUsers` uses `SearchPeople` internally and `SearchTweets` accepts only Top and Latest.

## Write

| Method | Account | Description |
|--------|---------|-------------|
| `CreateTweet(ctx, acc, text)` | Given | Post a tweet; returns its ID. |
| `PostWithAccount(ctx, username, text)` | By username | `CreateTweet` for the pool account with that username (must be active). |
| `ReplyTweet(ctx, acc, tweetID, text)` | Given | Reply to a tweet; returns the reply ID. |
| `LikeTweet(ctx, acc, tweetID)` | Given | Like a tweet. |
| `UnlikeTweet(ctx, acc, tweetID)` | Given | Remove a like. |
| `Retweet(ctx, acc, tweetID)` | Given | Retweet; returns the new retweet ID. |
| `Unretweet(ctx, acc, tweetID)` | Given | Remove a retweet; takes the original tweet's ID. |
| `FollowUser(ctx, acc, userID)` | Given | Follow a user. One attempt, no retry. |
| `SetDeviceNotifications(ctx, acc, userID, on)` | Given | Turn notifications for a user on or off. One attempt, no retry. The returned flag is advisory; confirm with `FriendshipState` when it matters. |

## DM

| Method | Account | Description |
|--------|---------|-------------|
| `GetDMInbox(ctx)` | Pool | The pool account's DM conversations with the messages present in the initial inbox state (1.1 API). |
| `SendDM(ctx, acc, conversationID, text)` | Given | Send a text DM into an existing conversation; returns the message ID. `conversationID` is a conversation ID (1:1 form `<recipientID>-<selfID>`), not a user ID. |

## Media

| Method | Account | Description |
|--------|---------|-------------|
| `UploadMedia(ctx, acc, data, mimeType)` | Given | Chunked upload (INIT/APPEND/FINALIZE, polls async video/GIF processing); returns the media ID. |
| `UploadMediaFile(ctx, acc, path)` | Given | Reads a file, infers the MIME type, uploads it. |
| `CreateTweetWithMedia(ctx, acc, text, mediaIDs)` | Given | Post a tweet with uploaded media attached. |
| `PostWithMedia(ctx, username, text, paths...)` | By username | Upload the files and post them from the named account. |

## Pool and account helpers

| Method | Description |
|--------|-------------|
| `AccountByUsername(username)` | Look up a pool account (case-insensitive); nil if absent. |
| `HealthReport()` | Per-account totals, consecutive failures, and proxy backoff state. |
| `Pool()` | The underlying account pool. |
| `Close()` | Closes the idle connections of every HTTP client the `Client` built and unregisters its metrics callback, so a replacement client can report on the same `MeterProvider`. Does not stop in-flight requests or background refreshes; the `Client` stays usable. |
| `Account.SetPickPace(stealth.Jitter{Min, Max})` / `PickPace()` | Pool picks of the account are separated by a random gap in `[Min, Max)` (zero value = off, the default; `Max <= Min` = fixed `Min`). Other accounts serve meanwhile; a call with no other account waits for the gap. Runtime-safe; pool picks only (not account-pinned writes or media). See [Rate limits](rate-limits.md). |
| `WireFor(stealth.TLSProfile)` / `Account.Wire()` | The request shape an identity sends: `"chrome"` (`WireChrome`) for the current desktop Chrome profiles (chrome_146, 150, 152, 156), `"legacy"` (`WireLegacy`, the frozen v0.8.6 shape) for every other profile. The client applies this same rule; call it instead of copying the profile set. `Account.Wire()` follows the profile stated on the account, so an account no `Client` has completed yet and that states none reports `"legacy"`. |
| `ValidateAccount(ctx, acc)` | Check an account's credentials against the account settings endpoint. |
| `SetAutoReloginGate(gate)` | Replace the relogin circuit breaker (see below). |

The `social` package is a thin client for an external account-leasing service. `SearchWithSocial`, `SearchTweetsWithSocial`, `SearchUsersWithSocial`, `GetUserWithSocial`, `GetUserTweetsWithSocial` and `GetTweetConversationWithSocial`, along with `GetFollowersWithSocial`, `GetFollowingWithSocial`, `GetRetweetersWithSocial`, `GetTweetByIDWithSocial` and `GetUserTweetsByIDWithSocial`, lease an account, run the call, retry on other accounts, and report the outcome. The repeated header-less code 88 escalation described in [Error handling](errors.md) does not fire on this path: each attempt builds a fresh `Account`, so the count never reaches the threshold. A leased account that keeps answering 88 is reported to the service as `auth_error`.

A lease may carry an optional top-level `identity` object (`mode`, `tls_profile`, `user_agent`, `browser`, `os`; see `social.Identity`): the browser identity the account service presents that session as. When present, the helpers apply it as stated (TLS profile and User-Agent together), so one session is presented consistently by one browser identity whichever process uses it; nothing is derived from the username. A lease without it, or with an empty object, keeps the per-account identity derived from the username. The identity is validated when the lease is taken: one that lacks a `tls_profile` or `user_agent`, or names a TLS profile this go-stealth build does not know (`stealth.ErrUnknownTLSProfile`), is never served under a different browser. That lease is skipped with `ErrLeaseIdentity` and no usage report is sent for it (the credential is not at fault), and the helper tries the next lease; `errors.Is(err, ErrLeaseIdentity)` identifies the failure when every attempt is skipped.

The usage report that ends a call (the final success, or a deterministic outcome that is not retried) is sent in the background, after the helper has returned. Delivery is best effort and bounded: a report that cannot be queued is dropped and counted. A process that exits right after its last helper call can lose that report, so call `FlushSocialReports(ctx)` at shutdown, with a short deadline:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
_ = twitter.FlushSocialReports(ctx)
```

It returns `ctx.Err()` if reports are still in flight at the deadline.

A caller-side cancellation or deadline (`context.Canceled`, `context.DeadlineExceeded`, or the caller ctx ending mid-request) is NOT the leased account's fault: it reports nothing to the service (the vocabulary has no neutral status) and stops leasing immediately. Reports are sent on a detached, bounded context, so a `success` verdict still lands when the caller's ctx died before the response arrived.

## Tracing

`WithRequestTrace(ctx, trace)` attaches a `RequestTrace` that records per-phase timestamps for the requests made under that context. It exists for latency diagnosis, and the request path pays only a nil check when no trace is attached.

`SnowflakeTime(id)` returns the creation time encoded in a tweet ID.

## Types

The response types are defined in package `model` (`github.com/anatolykoptev/go-twitter/model`) and aliased in the root package, so `twitter.Tweet` and `model.Tweet` are the same type.

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
    Promoted                                     bool     // X marked the entry as promoted (an ad); skip it when reading a timeline
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
