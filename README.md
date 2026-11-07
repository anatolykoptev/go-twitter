# go-twitter

[![Go Reference](https://pkg.go.dev/badge/github.com/anatolykoptev/go-twitter.svg)](https://pkg.go.dev/github.com/anatolykoptev/go-twitter)
[![CI](https://github.com/anatolykoptev/go-twitter/actions/workflows/preflight.yml/badge.svg)](https://github.com/anatolykoptev/go-twitter/actions/workflows/preflight.yml)
[![Release](https://img.shields.io/github/v/release/anatolykoptev/go-twitter)](https://github.com/anatolykoptev/go-twitter/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

An unofficial Go client for the X (Twitter) web API.

Give it one or more logged-in sessions and call typed methods for timelines, search, profiles, posts and DMs. The client spreads requests across accounts, follows the rate limits X reports, picks up changed GraphQL operation IDs on its own, and exposes what it does through OpenTelemetry metrics.

## Install

```bash
go get github.com/anatolykoptev/go-twitter
```

Requires Go 1.27.3 or newer.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	twitter "github.com/anatolykoptev/go-twitter"
)

func main() {
	client, err := twitter.NewClient(twitter.ClientConfig{
		Accounts: []*twitter.Account{{
			Username:  "your_handle",
			AuthToken: os.Getenv("X_AUTH_TOKEN"),
			CT0:       os.Getenv("X_CT0"),
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	tweets, err := client.SearchTweets(context.Background(), "golang", twitter.SearchLatest, 20)
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range tweets {
		fmt.Printf("@%s: %s\n", t.AuthorHandle, t.Text)
	}
}
```

`auth_token` and `ct0` are the cookies of a logged-in x.com browser session. They give full access to the account, so keep them out of source code and logs.

## What it covers

| Area | Methods |
|------|---------|
| Profiles and tweets | `GetUserByScreenName`, `GetUserTweets`, `GetTweetByID`, `GetTweetConversation` |
| Timelines | `GetHomeTimeline`, `GetHomeLatestTimeline`, `GetListTweets`, `GetCommunityTweets`, `GetBookmarks` |
| Search | `SearchTweets`, `SearchUsers` |
| Relationships | `GetFollowers`, `GetFollowing`, `GetVerifiedFollowers`, `GetRetweeters`, `FriendshipState` |
| Notifications | `GetDeviceNotifications`, `SetDeviceNotifications` |
| Posting | `CreateTweet`, `ReplyTweet`, `LikeTweet`, `Retweet`, `FollowUser`, `UploadMedia` |
| Direct messages | `GetDMInbox`, `SendDM` |

The [API reference](docs/api.md) has the full list.

## Built for long-running services

- **Account pool.** Rotates accounts, tracks the health of each one, and moves on when an account fails or runs out of budget.
- **Rate limits.** Follows X's `x-rate-limit-*` headers per account and endpoint, with per-account spend shares, in-flight caps and request spacing.
- **Typed errors.** Every failure is an `*APIError` with predicates such as `IsRateLimited` and `IsLocked`.
- **Operation ID refresh.** Reads current GraphQL IDs from x.com's bundles at runtime, so a rotated ID recovers without a release.
- **Session storage.** Saves sessions to files by default, or anywhere else through the `SessionStore` interface.
- **Observability.** OpenTelemetry metrics, a request observer hook and per-request tracing.

## Status and limits

The project is pre-1.0, so the API can change between minor versions. It relies on web endpoints that X can change without notice. Known gaps:

- `Tweet` and `TwitterUser` are compact: there is no media, and no reply, retweet or quote markers yet ([#63](https://github.com/anatolykoptev/go-twitter/issues/63)).
- Timelines leave out pinned tweets ([#52](https://github.com/anatolykoptev/go-twitter/issues/52)).
- Pagination has no exported cursors ([#67](https://github.com/anatolykoptev/go-twitter/issues/67)).
- Rate-limit state lives in memory and resets on restart ([#66](https://github.com/anatolykoptev/go-twitter/issues/66)).
- x.com is moving to a new web client, and only part of it is handled so far ([#119](https://github.com/anatolykoptev/go-twitter/issues/119), [inventory](docs/xweb-migration.md)).
- Password login uses X's older flow and is not tested against the current one. Use session cookies.

## Documentation

| Guide | Contents |
|-------|----------|
| [API reference](docs/api.md) | Every method, its account handling, and the types |
| [Configuration](docs/configuration.md) | Accounts, `ClientConfig`, environment variables |
| [Error handling](docs/errors.md) | Recovery per error class, predicates, relogin breaker |
| [Rate limiting](docs/rate-limits.md) | Windows, spend shares, holds, pacing, request observer |
| [Session storage](docs/sessions.md) | The `SessionStore` contract and a database example |
| [Request format](docs/request-format.md) | What the client sends and how to configure it |
| [Metrics](docs/metrics.md) | OpenTelemetry instruments and attributes |
| [Maintenance](docs/maintenance.md) | Keeping GraphQL IDs current, tools, CI |

Release history is in the [changelog](CHANGELOG.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). To report a vulnerability, follow [SECURITY.md](SECURITY.md) instead of opening a public issue.

## Disclaimer

This is an independent project. It is not affiliated with, endorsed by or sponsored by X Corp. It uses X's web endpoints rather than the official X API. You are responsible for following X's terms and the laws that apply to your use.

## License

[MIT](LICENSE)

<!-- throwaway docs-only proof, do not merge -->
