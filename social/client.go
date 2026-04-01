// Package social provides a thin HTTP client for the go-social account management API.
package social

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/%s/account", c.baseURL, platform), nil)
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

// ReportUsage reports the result of using an account back to go-social.
func (c *Client) ReportUsage(ctx context.Context, platform, accountID, status string) error {
	body := fmt.Sprintf(`{"status":"%s"}`, status)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/report/%s", c.baseURL, platform, accountID),
		strings.NewReader(body))
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
