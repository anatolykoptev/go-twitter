package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/captcha"
	"github.com/pquerna/otp/totp"
)

// arkosePublicKey is Twitter's well-known FunCaptcha public key for login flows.
const arkosePublicKey = "0152B4EB-D2DC-460A-89A1-629838B529C9"

// sessionDir returns the directory for persisting session cookies.
func sessionDir(override string) string {
	if override != "" {
		return override
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".go-twitter", "sessions")
}

// sessionPath returns the file path for a given username's session.
func sessionPath(dir, username string) string {
	return filepath.Join(dir, username+".json")
}

// savedSession holds serialized cookie data for persistence.
type savedSession struct {
	AuthToken string    `json:"auth_token"`
	CT0       string    `json:"ct0"`
	SavedAt   time.Time `json:"saved_at"`
}

// saveSession persists auth_token and ct0 to disk.
func saveSession(dir, username, authToken, ct0 string) error {
	d := sessionDir(dir)
	if err := os.MkdirAll(d, 0700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	s := savedSession{AuthToken: authToken, CT0: ct0, SavedAt: time.Now()}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := sessionPath(d, username)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write session %s: %w", path, err)
	}
	slog.Debug("session saved", slog.String("user", username))
	return nil
}

// loadSession loads a persisted session from disk.
func loadSession(dir, username string, ttl time.Duration) (authToken, ct0 string, err error) {
	data, err := os.ReadFile(sessionPath(sessionDir(dir), username))
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", err
	}
	var s savedSession
	if err := json.Unmarshal(data, &s); err != nil {
		return "", "", err
	}
	if time.Since(s.SavedAt) > ttl {
		slog.Debug("session expired", slog.String("user", username))
		return "", "", nil
	}
	return s.AuthToken, s.CT0, nil
}

// relogin clears auth credentials and performs a fresh login.
func (c *Client) relogin(acc *Account) error {
	slog.Info("attempting relogin", slog.String("user", acc.Username))

	bc := c.clientForAccount(acc)

	acc.SetCredentials("", "")
	_ = os.Remove(sessionPath(sessionDir(c.cfg.SessionDir), acc.Username))

	if err := c.loadOrLogin(acc, bc); err != nil {
		return fmt.Errorf("relogin %s: %w", acc.Username, err)
	}

	acc.Reset()
	slog.Info("relogin succeeded", slog.String("user", acc.Username))
	return nil
}

// loadOrLogin attempts to load a persisted session, falling back to login.
func (c *Client) loadOrLogin(acc *Account, client *stealth.BrowserClient) error {
	authToken, ct0, err := loadSession(c.cfg.SessionDir, acc.Username, c.cfg.SessionTTL)
	if err != nil {
		slog.Warn("error loading session", slog.String("user", acc.Username), slog.Any("error", err))
	}
	if authToken != "" && ct0 != "" {
		acc.AuthToken = authToken
		acc.CT0 = ct0
		acc.ct0RefreshedAt = time.Now()
		slog.Info("loaded session from disk", slog.String("user", acc.Username))
		return nil
	}

	if acc.AuthToken != "" && acc.CT0 != "" {
		acc.ct0RefreshedAt = time.Now()
		slog.Info("using provided credentials", slog.String("user", acc.Username))
		if err := saveSession(c.cfg.SessionDir, acc.Username, acc.AuthToken, acc.CT0); err != nil {
			slog.Warn("session save failed", slog.String("user", acc.Username), slog.Any("error", err))
		}
		return nil
	}

	if acc.Password == "" {
		return fmt.Errorf("no session and no password for account %s", acc.Username)
	}

	if err := c.login(acc, client); err != nil {
		return fmt.Errorf("login failed for %s: %w", acc.Username, err)
	}

	if err := saveSession(c.cfg.SessionDir, acc.Username, acc.AuthToken, acc.CT0); err != nil {
		slog.Warn("session save failed", slog.String("user", acc.Username), slog.Any("error", err))
	}
	return nil
}

// login performs Twitter's multi-step login flow.
func (c *Client) login(acc *Account, client *stealth.BrowserClient) error {
	slog.Info("logging in", slog.String("user", acc.Username))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	guestToken, err := c.getGuestToken(client)
	if err != nil {
		return fmt.Errorf("get guest token: %w", err)
	}

	fr, err := c.initLoginFlowFull(client, guestToken)
	if err != nil {
		return fmt.Errorf("init login flow: %w", err)
	}

	for round := 0; round < 10; round++ {
		if len(fr.Subtasks) == 0 {
			break
		}

		subtaskID := fr.Subtasks[0].SubtaskID
		slog.Debug("login subtask", slog.String("user", acc.Username), slog.String("subtask", subtaskID))

		switch subtaskID {
		case "LoginJsInstrumentationSubtask":
			fr, err = c.submitJsInstrumentation(client, guestToken, fr.FlowToken)

		case "LoginEnterUserIdentifierSSO":
			fr, err = c.submitUsernameStep(client, guestToken, fr.FlowToken, acc.Username)

		case "LoginEnterPassword":
			fr, err = c.submitPasswordStep(client, guestToken, fr.FlowToken, acc.Password)

		case "LoginArkoseChallenge", "LoginArkoseCaptcha", "LoginEnterRecaptcha":
			if c.cfg.CaptchaSolver == nil {
				return fmt.Errorf("CAPTCHA required but no solver configured for %s", acc.Username)
			}
			token, solveErr := c.cfg.CaptchaSolver.Solve(ctx, arkosePublicKey, "https://twitter.com")
			if solveErr != nil {
				return fmt.Errorf("CAPTCHA solve failed for %s: %w", acc.Username, solveErr)
			}
			slog.Info("CAPTCHA solved for login", slog.String("user", acc.Username))
			fr, err = c.submitCaptchaStep(client, guestToken, fr.FlowToken, token)

		case "LoginTwoFactorAuthChallenge":
			if acc.TOTPSecret == "" {
				return fmt.Errorf("2FA required but no TOTP secret for %s", acc.Username)
			}
			code, codeErr := totp.GenerateCode(acc.TOTPSecret, time.Now())
			if codeErr != nil {
				return fmt.Errorf("TOTP code generation failed for %s: %w", acc.Username, codeErr)
			}
			slog.Info("submitting TOTP code", slog.String("user", acc.Username))
			fr, err = c.submitTOTPStep(client, guestToken, fr.FlowToken, code)

		case "LoginEnterAlternateIdentifierSubtask":
			fr, err = c.submitAlternateIdentifier(client, guestToken, fr.FlowToken, acc.Username)

		case "LoginSuccessSubtask", "AccountDuplicationCheck":
			slog.Debug("login flow complete", slog.String("user", acc.Username), slog.String("terminal", subtaskID))
			goto done

		case "DenyLoginSubtask":
			return fmt.Errorf("login denied for %s (account may be locked or disabled)", acc.Username)

		default:
			slog.Warn("unknown login subtask, skipping", slog.String("user", acc.Username), slog.String("subtask", subtaskID))
			fr, err = c.submitGenericStep(client, guestToken, fr.FlowToken, subtaskID)
		}

		if err != nil {
			return fmt.Errorf("login subtask %s for %s: %w", subtaskID, acc.Username, err)
		}
	}

done:
	authToken := client.GetCookieValue("https://api.twitter.com", "auth_token")
	if authToken == "" {
		authToken = client.GetCookieValue("https://twitter.com", "auth_token")
	}
	ct0 := client.GetCookieValue("https://api.twitter.com", "ct0")
	if ct0 == "" {
		ct0 = client.GetCookieValue("https://twitter.com", "ct0")
	}
	if ct0 == "" {
		ct0 = GenerateCT0()
	}

	if authToken == "" {
		return fmt.Errorf("login completed but no auth_token in cookies for %s", acc.Username)
	}

	acc.SetCredentials(authToken, ct0)
	slog.Info("login successful", slog.String("user", acc.Username))
	return nil
}

// loginOpenAccount creates an anonymous Twitter session.
func (c *Client) loginOpenAccount(ctx context.Context) (*Account, error) {
	bc, err := stealth.NewClient(
		stealth.WithHeaderOrder(twitterHeaderOrder),
	)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}

	guestToken, err := c.getGuestToken(bc)
	if err != nil {
		return nil, fmt.Errorf("guest token: %w", err)
	}

	headers := loginFlowHeaders(guestToken, "")
	body, _, status, err := bc.DoWithHeaderOrder("POST",
		twitterAPIURL+"/1.1/onboarding/task.json?flow_name=welcome",
		headers, strings.NewReader(openAccountPayload), twitterHeaderOrder,
	)
	if err != nil {
		return nil, fmt.Errorf("welcome init: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("welcome init HTTP %d: %s", status, string(body[:min(200, len(body))]))
	}

	var flowResp struct {
		FlowToken string `json:"flow_token"`
		Subtasks  []struct {
			SubtaskID string `json:"subtask_id"`
		} `json:"subtasks"`
	}
	if err := json.Unmarshal(body, &flowResp); err != nil {
		return nil, fmt.Errorf("parse welcome flow: %w", err)
	}
	flowToken := flowResp.FlowToken

	for _, st := range flowResp.Subtasks {
		if st.SubtaskID == "LoginJsInstrumentationSubtask" {
			payload := fmt.Sprintf(`{"flow_token":%q,"subtask_inputs":[{"subtask_id":"LoginJsInstrumentationSubtask","js_instrumentation":{"response":"{\"rf\":{\"a\":\"b\"},\"s\":\"s\"}","link":"next_link"}}]}`, flowToken)
			body2, _, status2, err := bc.DoWithHeaderOrder("POST",
				twitterAPIURL+"/1.1/onboarding/task.json",
				headers, strings.NewReader(payload), twitterHeaderOrder,
			)
			if err != nil {
				return nil, fmt.Errorf("js instrumentation: %w", err)
			}
			if status2 != 200 {
				return nil, fmt.Errorf("js instrumentation HTTP %d: %s", status2, string(body2[:min(200, len(body2))]))
			}
			var resp2 struct {
				FlowToken string `json:"flow_token"`
			}
			if err := json.Unmarshal(body2, &resp2); err == nil && resp2.FlowToken != "" {
				flowToken = resp2.FlowToken
			}
			break
		}
	}

	_ = flowToken

	authToken := bc.GetCookieValue("https://api.twitter.com", "auth_token")
	if authToken == "" {
		authToken = bc.GetCookieValue("https://twitter.com", "auth_token")
	}
	ct0 := bc.GetCookieValue("https://api.twitter.com", "ct0")
	if ct0 == "" {
		ct0 = bc.GetCookieValue("https://twitter.com", "ct0")
	}

	if authToken == "" {
		return nil, fmt.Errorf("open account: no auth_token in cookies after welcome flow")
	}

	username := "guest_" + guestToken[:min(8, len(guestToken))]
	slog.Info("open account created", slog.String("username", username))
	return &Account{
		Username:  username,
		AuthToken: authToken,
		CT0:       ct0,
		active:    true,
	}, nil
}

// getGuestToken fetches a Twitter guest token.
func (c *Client) getGuestToken(client *stealth.BrowserClient) (string, error) {
	headers := map[string]string{
		"authorization": "Bearer " + BearerToken,
		"content-type":  "application/json",
		"user-agent":    defaultUserAgent,
	}
	body, _, status, err := client.DoWithHeaderOrder("POST", twitterAPIURL+"/1.1/guest/activate.json", headers, nil, twitterHeaderOrder)
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("guest token: HTTP %d", status)
	}
	var resp struct {
		GuestToken string `json:"guest_token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}
	if resp.GuestToken == "" {
		return "", fmt.Errorf("empty guest token in response")
	}
	return resp.GuestToken, nil
}

// acquireGuestToken fetches a fresh guest token with exponential backoff.
func (c *Client) acquireGuestToken(ctx context.Context, client *stealth.BrowserClient) (string, error) {
	backoff := stealth.BackoffConfig{
		InitialWait: 2 * time.Second,
		MaxWait:     60 * time.Second,
		Multiplier:  2.0,
		JitterPct:   0.3,
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			delay := backoff.Duration(attempt)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}
		token, err := c.getGuestToken(client)
		if err == nil {
			return token, nil
		}
		lastErr = err
		slog.Warn("guest token acquisition failed", slog.Int("attempt", attempt+1), slog.Any("error", err))
	}
	return "", fmt.Errorf("acquire guest token after 3 attempts: %w", lastErr)
}

// openAccountPayload is the subtask_versions body for flow_name=welcome.
const openAccountPayload = `{"input_flow_data":{"flow_context":{"debug_overrides":{},"start_location":{"location":"splash_screen"}}},"subtask_versions":{"action_list":2,"alert_dialog":1,"app_download_cta":1,"check_logged_in_account":1,"choice_selection":3,"contacts_live_sync_permission_prompt":0,"cta":7,"email_verification":2,"end_flow":1,"enter_date":1,"enter_email":2,"enter_password":5,"enter_phone":2,"enter_recaptcha":1,"enter_text":5,"enter_username":2,"generic_urt":3,"in_app_notification":1,"interest_picker":3,"js_instrumentation":1,"menu_dialog":1,"notifications_permission_prompt":2,"open_account":2,"open_home_timeline":1,"open_link":1,"phone_verification":4,"privacy_options":1,"security_key":3,"select_avatar":4,"select_banner":2,"settings_list":7,"show_code":1,"sign_up":2,"sign_up_review":4,"tweet_selection_urt":1,"update_users":1,"upload_media":1,"user_recommendations_list":4,"user_recommendations_urt":1,"wait_spinner":3,"web_modal":1}}`

type flowResponse struct {
	FlowToken string        `json:"flow_token"`
	Subtasks  []flowSubtask `json:"subtasks"`
}

type flowSubtask struct {
	SubtaskID string `json:"subtask_id"`
}

func parseFlowResponse(body []byte) (*flowResponse, error) {
	var fr flowResponse
	if err := json.Unmarshal(body, &fr); err != nil {
		return nil, fmt.Errorf("parse flow response: %w", err)
	}
	if fr.FlowToken == "" {
		return nil, fmt.Errorf("empty flow_token in response: %s", string(body[:min(200, len(body))]))
	}
	return &fr, nil
}

func (c *Client) initLoginFlowFull(client *stealth.BrowserClient, guestToken string) (*flowResponse, error) {
	headers := loginFlowHeaders(guestToken, "")
	payload := `{"input_flow_data":{"flow_context":{"debug_overrides":{},"start_location":{"location":"splash_screen"}}},"subtask_versions":{"action_list":2,"alert_dialog":1,"app_download_cta":1,"check_logged_in_account":1,"choice_selection":3,"contacts_live_sync_permission_prompt":0,"cta":7,"email_verification":2,"end_flow":1,"enter_date":1,"enter_email":2,"enter_password":5,"enter_phone":2,"enter_recaptcha":1,"enter_text":5,"enter_username":2,"generic_urt":3,"in_app_notification":1,"interest_picker":3,"js_instrumentation":1,"menu_dialog":1,"notifications_permission_prompt":2,"open_account":2,"open_home_timeline":1,"open_link":1,"phone_verification":4,"privacy_options":1,"security_key":3,"select_avatar":4,"select_banner":2,"settings_list":7,"show_code":1,"sign_up":2,"sign_up_review":4,"tweet_selection_urt":1,"update_users":1,"upload_media":1,"user_recommendations_list":4,"user_recommendations_urt":1,"wait_spinner":3,"web_modal":1}}`

	body, _, status, err := client.DoWithHeaderOrder("POST",
		twitterAPIURL+"/1.1/onboarding/task.json?flow_name=login",
		headers,
		strings.NewReader(payload),
		twitterHeaderOrder,
	)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("init flow: HTTP %d: %s", status, string(body))
	}
	return parseFlowResponse(body)
}

func (c *Client) submitFlowStep(client *stealth.BrowserClient, guestToken, payload string) (*flowResponse, error) {
	headers := loginFlowHeaders(guestToken, "")
	body, _, status, err := client.DoWithHeaderOrder("POST",
		twitterAPIURL+"/1.1/onboarding/task.json",
		headers,
		strings.NewReader(payload),
		twitterHeaderOrder,
	)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("flow step HTTP %d: %s", status, string(body[:min(300, len(body))]))
	}
	return parseFlowResponse(body)
}

func (c *Client) submitUsernameStep(client *stealth.BrowserClient, guestToken, flowToken, username string) (*flowResponse, error) {
	payload := fmt.Sprintf(`{"flow_token":%q,"subtask_inputs":[{"subtask_id":"LoginEnterUserIdentifierSSO","settings_list":{"setting_responses":[{"key":"user_identifier","response_data":{"text_data":{"result":%q}}}],"link":"next_link"}}]}`,
		flowToken, username)
	return c.submitFlowStep(client, guestToken, payload)
}

func (c *Client) submitPasswordStep(client *stealth.BrowserClient, guestToken, flowToken, password string) (*flowResponse, error) {
	payload := fmt.Sprintf(`{"flow_token":%q,"subtask_inputs":[{"subtask_id":"LoginEnterPassword","enter_password":{"password":%q,"link":"next_link"}}]}`,
		flowToken, password)
	return c.submitFlowStep(client, guestToken, payload)
}

func (c *Client) submitJsInstrumentation(client *stealth.BrowserClient, guestToken, flowToken string) (*flowResponse, error) {
	payload := fmt.Sprintf(`{"flow_token":%q,"subtask_inputs":[{"subtask_id":"LoginJsInstrumentationSubtask","js_instrumentation":{"response":"{\"rf\":{\"a\":\"b\"},\"s\":\"s\"}","link":"next_link"}}]}`,
		flowToken)
	return c.submitFlowStep(client, guestToken, payload)
}

func (c *Client) submitCaptchaStep(client *stealth.BrowserClient, guestToken, flowToken, captchaToken string) (*flowResponse, error) {
	payload := fmt.Sprintf(`{"flow_token":%q,"subtask_inputs":[{"subtask_id":"LoginArkoseChallenge","web_modal":{"completion_deeplink":"twitter://onboarding/web_modal/next_link?access_token=%s"}}]}`,
		flowToken, captchaToken)
	return c.submitFlowStep(client, guestToken, payload)
}

func (c *Client) submitTOTPStep(client *stealth.BrowserClient, guestToken, flowToken, code string) (*flowResponse, error) {
	payload := fmt.Sprintf(`{"flow_token":%q,"subtask_inputs":[{"subtask_id":"LoginTwoFactorAuthChallenge","enter_text":{"text":%q,"link":"next_link"}}]}`,
		flowToken, code)
	return c.submitFlowStep(client, guestToken, payload)
}

func (c *Client) submitAlternateIdentifier(client *stealth.BrowserClient, guestToken, flowToken, identifier string) (*flowResponse, error) {
	payload := fmt.Sprintf(`{"flow_token":%q,"subtask_inputs":[{"subtask_id":"LoginEnterAlternateIdentifierSubtask","enter_text":{"text":%q,"link":"next_link"}}]}`,
		flowToken, identifier)
	return c.submitFlowStep(client, guestToken, payload)
}

func (c *Client) submitGenericStep(client *stealth.BrowserClient, guestToken, flowToken, subtaskID string) (*flowResponse, error) {
	payload := fmt.Sprintf(`{"flow_token":%q,"subtask_inputs":[{"subtask_id":%q,"action_list":{"link":"next_link"}}]}`,
		flowToken, subtaskID)
	return c.submitFlowStep(client, guestToken, payload)
}

// Ensure captcha import is used
var _ captcha.Solver
