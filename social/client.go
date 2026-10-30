// Package social provides a thin HTTP client for the go-social account management API.
package social

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin HTTP client for the go-social account management API.
type Client struct {
	baseURL  string
	token    string
	consumer string
	http     *http.Client
}

// NewClient creates a new go-social API client.
func NewClient(baseURL, token, consumer string) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		token:    token,
		consumer: consumer,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Credentials holds a leased account's credentials returned by go-social.
type Credentials struct {
	ID          string            `json:"id"`
	Credentials map[string]string `json:"credentials"`
	Proxy       string            `json:"proxy"`
	ExpiresIn   int               `json:"expires_in"`
}

// AcquireAccount gets the next healthy account for the given platform.
func (c *Client) AcquireAccount(ctx context.Context, platform string) (*Credentials, error) {
	return c.AcquireAccountFor(ctx, platform, "")
}

// AcquireAccountFor is AcquireAccount with an endpoint hint: the name of the
// operation (for example "SearchTimeline") the lease is about to serve. It is
// sent as the optional ?endpoint= query parameter so the credential service can
// prefer an account with budget left on that endpoint. An empty endpoint sends
// no parameter, and a service that does not know the parameter ignores it.
func (c *Client) AcquireAccountFor(ctx context.Context, platform, endpoint string) (*Credentials, error) {
	u := fmt.Sprintf("%s/%s/account", c.baseURL, platform)
	if endpoint != "" {
		u += "?" + url.Values{"endpoint": {endpoint}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Consumer", c.consumer)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("go-social request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("go-social returned %d", resp.StatusCode)
	}

	var creds Credentials
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		return nil, fmt.Errorf("decode go-social response: %w", err)
	}
	return &creds, nil
}

// Sentinel errors of AcquireNamedAccount, matched with errors.Is.
var (
	// ErrAccountNotFound: go-social does not know the requested username (404).
	ErrAccountNotFound = errors.New("go-social: account not found")
	// ErrAccountUnavailable: the account exists but cannot be leased now, or the
	// name is ambiguous (409).
	ErrAccountUnavailable = errors.New("go-social: account unavailable or ambiguous")
	// ErrAccountMismatch: a 200 answered with a different account than requested.
	ErrAccountMismatch = errors.New("go-social: leased a different account than requested")
)

// AcquireNamedAccount leases exactly the account named by username: the
// service pins the lease (?username=) and never substitutes another — 404
// (ErrAccountNotFound) for unknown, 409 (ErrAccountUnavailable) for
// unavailable/ambiguous. A 200 whose returned username does not match the
// requested one (case-insensitive) is ErrAccountMismatch: silently holding a
// different account would run the caller's work under the wrong identity. The
// wrongly leased account is not reported back: the usage API has no neutral
// status (any verdict would mis-bill a healthy account), so that lease simply
// runs out at its expires_in.
func (c *Client) AcquireNamedAccount(ctx context.Context, platform, username string) (*Credentials, error) {
	u := fmt.Sprintf("%s/%s/account?%s", c.baseURL, platform, url.Values{"username": {username}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Consumer", c.consumer)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("go-social request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("go-social returned %d for %q: %w", resp.StatusCode, username, ErrAccountNotFound)
	case http.StatusConflict:
		return nil, fmt.Errorf("go-social returned %d for %q: %w", resp.StatusCode, username, ErrAccountUnavailable)
	default:
		return nil, fmt.Errorf("go-social returned %d", resp.StatusCode)
	}

	var creds Credentials
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		return nil, fmt.Errorf("decode go-social response: %w", err)
	}
	if got := creds.Credentials["username"]; !strings.EqualFold(got, username) {
		return nil, fmt.Errorf("go-social leased a different account than %q: %w", username, ErrAccountMismatch)
	}
	return &creds, nil
}

// RateLimit is the last X rate-limit window a lease observed for one endpoint,
// taken from the x-rate-limit-limit / -remaining / -reset response headers.
type RateLimit struct {
	// Endpoint is the X operation name, for example "SearchTimeline".
	Endpoint string `json:"endpoint"`
	// Limit is the per-window request ceiling; omitted when X did not send it.
	Limit int `json:"limit,omitempty"`
	// Remaining is the number of requests left in the window (0 is meaningful).
	Remaining int `json:"remaining"`
	// Reset is the window end as a unix timestamp in seconds.
	Reset int64 `json:"reset"`
}

// ReportUsage reports the result of using an account back to go-social.
func (c *Client) ReportUsage(ctx context.Context, platform, accountID, status string) error {
	return c.ReportUsageWithRateLimit(ctx, platform, accountID, status, nil)
}

// ReportUsageWithRateLimit is ReportUsage plus the rate-limit window the lease
// observed, sent as {"status":..., "rate_limit":{endpoint,limit,remaining,reset}}.
// A nil rl sends the legacy body, so a service that predates the field sees
// exactly what ReportUsage always sent; one that does not know the field
// ignores it.
func (c *Client) ReportUsageWithRateLimit(ctx context.Context, platform, accountID, status string, rl *RateLimit) error {
	// Build the JSON body via Marshal (not Sprintf) so a status containing a
	// quote/backslash can't break out of the JSON string, and PathEscape the
	// interpolated path segments so they can't break the URL path.
	payload := struct {
		Status    string     `json:"status"`
		RateLimit *RateLimit `json:"rate_limit,omitempty"`
	}{Status: status, RateLimit: rl}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal report body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/report/%s", c.baseURL, url.PathEscape(platform), url.PathEscape(accountID)),
		strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("go-social report failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("go-social report returned %d", resp.StatusCode)
	}
	return nil
}
