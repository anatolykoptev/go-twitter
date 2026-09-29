package twitter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/social"
)

const maxSocialRetries = 6

// SearchWithSocial acquires accounts from go-social and searches Twitter on the
// Latest tab, retrying with different accounts on failure. go-social rotates
// accounts on each call, so retries naturally use different credentials.
func SearchWithSocial(ctx context.Context, sc *social.Client, query string, limit int) ([]*Tweet, error) {
	return withSocialAccount(ctx, sc, func(ctx context.Context, tw *Client) ([]*Tweet, error) {
		return tw.SearchTimeline(ctx, query, limit)
	})
}

// SearchTweetsWithSocial searches tweets on an explicit tab (Top or Latest).
func SearchTweetsWithSocial(ctx context.Context, sc *social.Client, query string, product SearchProduct, limit int) ([]*Tweet, error) {
	// Validate before acquiring — a typo'd product must not burn pool retries.
	if product != SearchTop && product != SearchLatest {
		return nil, fmt.Errorf("SearchTweets: invalid product %q (want Top or Latest)", product)
	}
	return withSocialAccount(ctx, sc, func(ctx context.Context, tw *Client) ([]*Tweet, error) {
		return tw.SearchTweets(ctx, query, product, limit)
	})
}

// SearchUsersWithSocial searches the People tab.
func SearchUsersWithSocial(ctx context.Context, sc *social.Client, query string, limit int) ([]*TwitterUser, error) {
	return withSocialAccount(ctx, sc, func(ctx context.Context, tw *Client) ([]*TwitterUser, error) {
		return tw.SearchUsers(ctx, query, limit)
	})
}

// GetUserWithSocial fetches a profile by handle.
func GetUserWithSocial(ctx context.Context, sc *social.Client, handle string) (*TwitterUser, error) {
	return withSocialAccount(ctx, sc, func(ctx context.Context, tw *Client) (*TwitterUser, error) {
		return tw.GetUserByScreenName(ctx, handle)
	})
}

// GetUserTweetsWithSocial resolves the handle and fetches its recent tweets —
// two GraphQL calls inside one account attempt.
func GetUserTweetsWithSocial(ctx context.Context, sc *social.Client, handle string, limit int) ([]*Tweet, error) {
	return withSocialAccount(ctx, sc, func(ctx context.Context, tw *Client) ([]*Tweet, error) {
		u, err := tw.GetUserByScreenName(ctx, handle)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", handle, err)
		}
		return tw.GetUserTweets(ctx, u.ID, limit)
	})
}

// GetTweetConversationWithSocial fetches the focal tweet and its conversation
// entries (replies/ancestors in page order).
func GetTweetConversationWithSocial(ctx context.Context, sc *social.Client, tweetID string) (*Tweet, []*Tweet, error) {
	type conv struct {
		focal   *Tweet
		replies []*Tweet
	}
	res, err := withSocialAccount(ctx, sc, func(ctx context.Context, tw *Client) (conv, error) {
		f, r, err := tw.GetTweetConversation(ctx, tweetID)
		return conv{f, r}, err
	})
	return res.focal, res.replies, err
}

// withSocialAccount runs fn on a client built from a freshly acquired account,
// retrying across accounts on failure — the acquire→call→ReportUsage loop
// SearchWithSocial grew organically, generalized.
func withSocialAccount[T any](ctx context.Context, sc *social.Client, fn func(context.Context, *Client) (T, error)) (T, error) {
	var lastErr error
	var zero T
	for attempt := range maxSocialRetries {
		res, err := tryWithAccount(ctx, sc, fn)
		if err == nil {
			return res, nil
		}
		if errors.Is(err, ErrNotFound) {
			return zero, err
		}
		lastErr = err
		slog.Warn("social request attempt failed, retrying",
			slog.Int("attempt", attempt+1),
			slog.Int("max", maxSocialRetries),
			slog.Any("error", err))
	}
	return zero, fmt.Errorf("all %d accounts failed: %w", maxSocialRetries, lastErr)
}

// tryWithAccount acquires one account, runs fn, and reports the result.
func tryWithAccount[T any](ctx context.Context, sc *social.Client, fn func(context.Context, *Client) (T, error)) (T, error) {
	var zero T
	creds, err := sc.AcquireAccount(ctx, "twitter")
	if err != nil {
		return zero, fmt.Errorf("acquire account: %w", err)
	}

	acc := &Account{
		Username:  creds.Credentials["username"],
		AuthToken: creds.Credentials["auth_token"],
		CT0:       creds.Credentials["ct0"],
	}

	tw, err := NewClient(ClientConfig{
		Accounts:     []*Account{acc},
		DefaultProxy: creds.Proxy,
		RateLimit:    ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute},
	})
	if err != nil {
		_ = sc.ReportUsage(ctx, "twitter", creds.ID, "auth_error")
		return zero, fmt.Errorf("create client for %s: %w", acc.Username, err)
	}

	res, err := fn(ctx, tw)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The account completed the request fine — the resource is just
			// absent. Healthy account, deterministic miss: report success and
			// propagate untouched so withSocialAccount doesn't retry.
			_ = sc.ReportUsage(ctx, "twitter", creds.ID, "success")
			return zero, err
		}
		_ = sc.ReportUsage(ctx, "twitter", creds.ID, "auth_error")
		return zero, fmt.Errorf("%s: %w", acc.Username, err)
	}

	_ = sc.ReportUsage(ctx, "twitter", creds.ID, "success")
	return res, nil
}
