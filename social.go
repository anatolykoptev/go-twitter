package twitter

import (
	"context"
	"fmt"
	"time"

	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/social"
)

// SearchWithSocial acquires an account from go-social, creates an ephemeral
// client, searches Twitter, and reports the result back to go-social.
// This is the recommended way to use go-twitter with the go-social account pool.
func SearchWithSocial(ctx context.Context, sc *social.Client, query string, limit int) ([]*Tweet, error) {
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
		return nil, fmt.Errorf("create ephemeral client: %w", err)
	}

	tweets, err := tw.SearchTimeline(ctx, query, limit)
	if err != nil {
		_ = sc.ReportUsage(ctx, "twitter", creds.ID, "auth_error")
		return nil, fmt.Errorf("social search: %w", err)
	}

	_ = sc.ReportUsage(ctx, "twitter", creds.ID, "success")
	return tweets, nil
}
