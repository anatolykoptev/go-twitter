# go-twitter

[![Go 1.26+](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Twitter/X scraping library for Go — multi-account pool, GraphQL API, anti-ban, CAPTCHA solving, TOTP 2FA, and session persistence.

Built on [go-stealth](https://github.com/anatolykoptev/go-stealth) for TLS fingerprinting, proxy rotation, and rate limiting.

## Features

- **Account Pool** — round-robin rotation with per-account health tracking and rate limits
- **GraphQL API** — users, tweets, followers, following, retweeters, search, post
- **Anti-Ban** — TLS fingerprinting, header ordering, client hints, x-client-transaction-id (xtid)
- **Auth** — multi-step login flow with password, TOTP 2FA, CAPTCHA (Capsolver)
- **Error Recovery** — CSRF rotation, session refresh, ban cooldown, guest token fallback
- **Session Persistence** — JSON file cache with TTL
- **Proxy Support** — per-account proxy, automatic backoff on failures

## Install

```bash
go get github.com/anatolykoptev/go-twitter
```

## Quick Start

```go
cfg := &twitter.ClientConfig{
    Accounts: []*twitter.Account{
        {Username: "user1", Password: "pass1", TOTPSecret: "BASE32SECRET"},
        {Username: "user2", AuthToken: "existing_token"},
    },
    DefaultProxy:  "http://proxy:8080",
    CaptchaSolver: captcha.NewCapsolver(os.Getenv("CAPSOLVER_KEY")),
}

client, err := twitter.NewClient(cfg)

// Read
user, _ := client.GetUserByScreenName(ctx, "elonmusk")
tweets, _ := client.GetUserTweets(ctx, user.ID, 50)
followers, _ := client.GetFollowers(ctx, user.ID, 100)

// Search
results, _ := client.SearchTimeline(ctx, "$BTC", 20)

// Write (account-specific)
tweetID, _ := client.PostWithAccount(ctx, "user1", "Hello Twitter!")
```

## API

| Method | Auth | Description |
|--------|------|-------------|
| `GetUserByScreenName` | Guest/Auth | Get user profile |
| `GetUserTweets` | Guest/Auth | Get user's tweets |
| `GetFollowers` | Auth | Paginated follower list |
| `GetFollowing` | Auth | Paginated following list |
| `GetRetweeters` | Auth | Users who retweeted |
| `SearchTimeline` | Auth | Search tweets |
| `CreateTweet` | Auth | Post a tweet |
| `PostWithAccount` | Auth | Post from specific account |

## Error Handling

Automatic recovery per error class:

| Error | Code | Action |
|-------|------|--------|
| CSRF mismatch | 353 | Rotate CT0 token, retry |
| Auth expired | 32 | Re-login |
| Rate limited | 429 | Back off, mark endpoint |
| Banned | 88 | Soft-deactivate 6h |
| Suspended | 64 | Permanent deactivation |
| Locked | 326 | CAPTCHA unlock attempt |

## Anti-Detection

- **xtid** — x-client-transaction-id generated from page animation keys (auto-refresh 30min)
- **CT0** — CSRF token proactively rotated every 4h
- **TLS** — browser-grade JA3 fingerprints via go-stealth
- **Headers** — exact Chrome/Firefox/Safari header ordering
- **Jitter** — randomized delays between requests

## Types

```go
type TwitterUser struct {
    ID, Handle, DisplayName, Bio string
    Followers, Following, TweetCount int
    IsVerified bool
}

type Tweet struct {
    ID, AuthorID, Text string
    CreatedAt          time.Time
    Views, Likes, Retweets, Quotes int
    TokenMentions      []string // extracted $TICKER mentions
}
```

## License

MIT
