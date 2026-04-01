package twitter

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/social"
)

const maxSocialRetries = 6

// SearchWithSocial acquires accounts from go-social and searches Twitter,
// retrying with different accounts on failure. go-social rotates accounts
// on each call, so retries naturally use different credentials.
func SearchWithSocial(ctx context.Context, sc *social.Client, query string, limit int) ([]*Tweet, error) {
	var lastErr error
	for attempt := range maxSocialRetries {
		tweets, err := trySearchWithAccount(ctx, sc, query, limit)
		if err == nil {
			return tweets, nil
		}
		lastErr = err
		slog.Warn("social search attempt failed, retrying",
			slog.Int("attempt", attempt+1),
			slog.Int("max", maxSocialRetries),
			slog.Any("error", err))
	}
	return nil, fmt.Errorf("all %d accounts failed: %w", maxSocialRetries, lastErr)
}

// trySearchWithAccount acquires one account, searches, and reports the result.
func trySearchWithAccount(ctx context.Context, sc *social.Client, query string, limit int) ([]*Tweet, error) {
	creds, err := sc.AcquireAccount(ctx, "twitter")
	if err != nil {
		return nil, fmt.Errorf("acquire account: %w", err)
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
		return nil, fmt.Errorf("create client for %s: %w", acc.Username, err)
	}

	tweets, err := tw.SearchTimeline(ctx, query, limit)
	if err != nil {
		_ = sc.ReportUsage(ctx, "twitter", creds.ID, "auth_error")
		return nil, fmt.Errorf("%s: %w", acc.Username, err)
	}

	_ = sc.ReportUsage(ctx, "twitter", creds.ID, "success")
	return tweets, nil
}
