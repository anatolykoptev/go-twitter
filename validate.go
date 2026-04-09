package twitter

import (
	"context"
	"fmt"
)

// ValidateAccount checks that an account's auth_token and ct0 are still valid
// by calling the authenticated account/settings.json REST endpoint.
// Returns nil on HTTP 200, an error on 401/403, or a wrapped error on network failure.
func (c *Client) ValidateAccount(ctx context.Context, acc *Account) error {
	bc := c.clientForAccount(acc)
	authTok, ct0, ua := acc.Credentials()
	headers := twitterHeaders(authTok, ct0, ua)

	_, _, status, err := c.doRequest(bc, "GET", accountSettingsURL, headers)
	if err != nil {
		return fmt.Errorf("validate account %s: request failed: %w", acc.Username, err)
	}
	switch status {
	case 200:
		return nil
	case 401:
		return fmt.Errorf("validate account %s: unauthorized (401) — credentials expired", acc.Username)
	case 403:
		return fmt.Errorf("validate account %s: forbidden (403) — account blocked or suspended", acc.Username)
	default:
		return fmt.Errorf("validate account %s: unexpected HTTP %d", acc.Username, status)
	}
}
