package captcha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	capsolverAPI     = "https://api.capsolver.com"
	pollInterval     = 3 * time.Second
	solveTimeout     = 120 * time.Second
	balanceWarnLevel = 5.0 // warn when balance drops below $5
)

// Capsolver implements Solver using the Capsolver API.
type Capsolver struct {
	apiKey string
	client *http.Client
}

// NewCapsolver creates a Capsolver client with the given API key.
func NewCapsolver(apiKey string) *Capsolver {
	return &Capsolver{
		apiKey: apiKey,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Solve submits a FunCaptcha (Arkose Labs) challenge to Capsolver and polls for the result.
func (c *Capsolver) Solve(ctx context.Context, siteKey, pageURL string) (string, error) {
	// Check balance before solve
	bal, balErr := c.Balance(ctx)
	if balErr == nil && bal < balanceWarnLevel {
		slog.Warn("Capsolver balance low", slog.Float64("balance", bal))
	}

	// Create task
	taskReq := map[string]any{
		"clientKey": c.apiKey,
		"task": map[string]any{
			"type":             "FunCaptchaTaskProxyLess",
			"websiteURL":       pageURL,
			"websitePublicKey": siteKey,
		},
	}

	var createResp struct {
		ErrorID          int    `json:"errorId"`
		ErrorCode        string `json:"errorCode"`
		ErrorDescription string `json:"errorDescription"`
		TaskID           string `json:"taskId"`
	}
	if err := c.post(ctx, "/createTask", taskReq, &createResp); err != nil {
		return "", fmt.Errorf("capsolver createTask: %w", err)
	}
	if createResp.ErrorID != 0 {
		return "", fmt.Errorf("capsolver createTask error %s: %s", createResp.ErrorCode, createResp.ErrorDescription)
	}
	if createResp.TaskID == "" {
		return "", fmt.Errorf("capsolver: empty taskId in response")
	}

	slog.Info("CAPTCHA task created", slog.String("taskId", createResp.TaskID))

	// Poll for result
	deadline := time.Now().Add(solveTimeout)
	resultReq := map[string]any{
		"clientKey": c.apiKey,
		"taskId":    createResp.TaskID,
	}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("capsolver: solve timeout after %s", solveTimeout)
		}

		var resultResp struct {
			ErrorID          int    `json:"errorId"`
			ErrorCode        string `json:"errorCode"`
			ErrorDescription string `json:"errorDescription"`
			Status           string `json:"status"`
			Solution         struct {
				Token string `json:"token"`
			} `json:"solution"`
		}
		if err := c.post(ctx, "/getTaskResult", resultReq, &resultResp); err != nil {
			return "", fmt.Errorf("capsolver getTaskResult: %w", err)
		}
		if resultResp.ErrorID != 0 {
			return "", fmt.Errorf("capsolver result error %s: %s", resultResp.ErrorCode, resultResp.ErrorDescription)
		}

		switch resultResp.Status {
		case "ready":
			if resultResp.Solution.Token == "" {
				return "", fmt.Errorf("capsolver: ready but empty token")
			}
			slog.Info("CAPTCHA solved", slog.String("taskId", createResp.TaskID))
			return resultResp.Solution.Token, nil
		case "processing":
			select {
			case <-time.After(pollInterval):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		default:
			return "", fmt.Errorf("capsolver: unexpected status %q", resultResp.Status)
		}
	}
}

// Balance returns the Capsolver account balance in USD.
func (c *Capsolver) Balance(ctx context.Context) (float64, error) {
	req := map[string]any{"clientKey": c.apiKey}
	var resp struct {
		ErrorID int     `json:"errorId"`
		Balance float64 `json:"balance"`
	}
	if err := c.post(ctx, "/getBalance", req, &resp); err != nil {
		return 0, err
	}
	if resp.ErrorID != 0 {
		return 0, fmt.Errorf("capsolver balance error %d", resp.ErrorID)
	}
	return resp.Balance, nil
}

// post sends a JSON POST request to the Capsolver API and decodes the response.
func (c *Capsolver) post(ctx context.Context, path string, payload, result any) error {
	return c.postURL(ctx, capsolverAPI+path, payload, result)
}

// postURL sends a JSON POST to an arbitrary URL. Used by post() and tests.
func (c *Capsolver) postURL(ctx context.Context, url string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("capsolver HTTP %d: %s", resp.StatusCode, string(data[:min(200, len(data))]))
	}

	return json.Unmarshal(data, result)
}
