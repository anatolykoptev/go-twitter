package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/captcha"
	"github.com/anatolykoptev/go-twitter/internal/telemetry"
	"github.com/pquerna/otp/totp"
)

// arkosePublicKey is Twitter's well-known FunCaptcha public key for login flows.
const arkosePublicKey = "0152B4EB-D2DC-460A-89A1-629838B529C9"

// reloginOutcomeRecorder is implemented by gates (e.g. the default
// reloginBreaker) that track consecutive relogin outcomes per account. relogin()
// feeds the outcome back so the gate can trip after repeated failures and reset
// on success. A gate that does not implement it (e.g. go-social's external gate)
// simply receives no feedback — Allowed still governs.
type reloginOutcomeRecorder interface {
	RecordFailure(username string)
	RecordSuccess(username string)
}

// reloginFlight is one in-progress relogin of one account. Callers that arrive
// while it runs park on done and take its outcome (err, written before done is
// closed).
type reloginFlight struct {
	done    chan struct{}
	err     error
	waiters atomic.Int32
}

// errReloginShared marks a relogin error a waiter took from another caller's
// flight: the flight's leader already acted on it (benched the account).
var errReloginShared = errors.New("shared relogin outcome")

// errReloginAborted is a flight's outcome until doRelogin returns: a panic
// inside it leaves this in place, so waiters never read a half-done flight as
// a success.
var errReloginAborted = errors.New("relogin aborted")

// benchFailedRelogin takes acc out of rotation after its relogin failed. Only
// the flight leader benches: a waiter's error carries errReloginShared, and N
// callers of one failed relogin must produce one deactivation (and one
// item_deactivated alert), not N.
func (c *Client) benchFailedRelogin(acc *Account, reErr error) {
	if errors.Is(reErr, errReloginShared) {
		return
	}
	c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
}

// relogin replaces the account's credentials with a fresh login, single-flight
// per account: the first caller (the leader) runs the login; callers that
// arrive while it is in progress wait for its outcome and use it, so N
// requests that hit code 32 on one account cost one login and one gate/breaker
// outcome, and the in-memory credentials and the stored session cannot diverge
// between two concurrent logins. A waiter honours ctx: when it ends the waiter
// returns ctx.Err() and the flight carries on for the others. The login itself
// is not bound to any caller's ctx (it has its own timeout), so one caller
// giving up never fails the relogin the others share.
//
// failedTok is the auth token the caller's request was rejected with. When the
// account already holds a different token, a relogin that finished since then
// replaced it: the caller skips the login (returns nil) and retries with the
// current credentials.
func (c *Client) relogin(ctx context.Context, acc *Account, failedTok string) error {
	if cur, _, _ := acc.Credentials(); failedTok != "" && cur != failedTok {
		slog.Debug("relogin skipped: credentials already replaced", slog.String("user", acc.Username))
		return nil
	}
	c.reloginMu.Lock()
	if f, ok := c.reloginFlights[acc]; ok {
		f.waiters.Add(1)
		c.reloginMu.Unlock()
		select {
		case <-f.done:
			if f.err != nil {
				return fmt.Errorf("%w: %w", errReloginShared, f.err)
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f := &reloginFlight{done: make(chan struct{}), err: errReloginAborted}
	if c.reloginFlights == nil {
		c.reloginFlights = make(map[*Account]*reloginFlight)
	}
	c.reloginFlights[acc] = f
	c.reloginMu.Unlock()

	// Deferred so a panic in the login cannot leave waiters parked forever.
	defer func() {
		c.reloginMu.Lock()
		// Leaving the map and signalling done are one step: a caller between
		// the two would start a second login beside this one, and the two
		// could write the account's credentials and stored session out of order.
		delete(c.reloginFlights, acc)
		if c.reloginWindowHook != nil {
			c.reloginWindowHook()
		}
		close(f.done)
		c.reloginMu.Unlock()
	}()
	f.err = c.doRelogin(acc)
	return f.err
}

// doRelogin is one relogin attempt (see relogin for the single-flight wrapper).
// The gate is consulted BEFORE anything else, so an OPEN breaker neither
// touches a still-valid session nor hammers the login endpoint.
//
// The account's current credentials and stored session are kept until a
// replacement is in hand: the login runs on a candidate holding the identity
// (username, password, TOTP seed) but no tokens, and only its success replaces
// the account's tokens. A failed attempt — no password configured, a login
// challenge, a network error — leaves the account and the store exactly as they
// were. The account then keeps its old token, so every caller that sees this
// error benches the account (SoftDeactivate for AuthCooldown) to keep it out of
// rotation; the relogin itself does not.
//
// The store is never read here (skipStore): the stored entry still holds the
// token that just failed, so a reload would adopt it. A successful login
// overwrites that entry; if the overwrite fails the stale entry is dropped
// (see establishCredentials).
func (c *Client) doRelogin(acc *Account) error {
	if c.reloginGate != nil {
		if ok, reason := c.reloginGate.Allowed(context.Background(), acc.Username); !ok {
			slog.Warn("twitter: auto-relogin blocked by gate",
				slog.String("user", acc.Username), slog.String("reason", reason))
			c.metrics.recordRelogin(telemetry.OutcomeBreakerOpen)
			return fmt.Errorf("relogin blocked: %s", reason)
		}
	}
	rec, _ := c.reloginGate.(reloginOutcomeRecorder)
	slog.Info("attempting relogin", slog.String("user", acc.Username))

	bc, err := c.clientForAccount(acc)
	if err != nil {
		// An account with no transport must neither lose its session nor
		// attempt a direct login.
		return err
	}

	cand := &Account{
		Username:           acc.Username,
		Password:           acc.Password,
		TOTPSecret:         acc.TOTPSecret,
		credsAuthoritative: acc.credsAuthoritative,
	}
	if err := c.establishCredentials(cand, bc, true); err != nil {
		if rec != nil {
			rec.RecordFailure(acc.Username)
		}
		c.metrics.recordRelogin(telemetry.OutcomeFailed)
		return fmt.Errorf("relogin %s: %w", acc.Username, err)
	}

	authToken, ct0, _ := cand.Credentials()
	acc.SetCredentials(authToken, ct0)
	if rec != nil {
		rec.RecordSuccess(acc.Username)
	}
	acc.Reset()
	c.metrics.recordRelogin(telemetry.OutcomeOK)
	slog.Info("relogin succeeded", slog.String("user", acc.Username))
	return nil
}

// loadOrLogin establishes the account's credentials.
//
// Accounts flagged credsAuthoritative (set only by tryWithAccount: the
// credential service leases fresh tokens on every call) use the provided pair
// and never touch the session file — it is neither read nor written here, so a
// stale file cannot override a leased token.
//
// Every other account (ParseAccounts, direct Account literals) keeps the
// original order, which long-lived clients rely on: an unexpired on-disk
// session first (it holds the newest token after an in-process relogin, so a
// restart must not fall back to the stale static config token), then provided
// credentials, then a fresh password login.
func (c *Client) loadOrLogin(acc *Account, client *stealth.BrowserClient) error {
	return c.establishCredentials(acc, client, false)
}

// establishCredentials is loadOrLogin with an escape hatch: skipStore bypasses
// the stored-session read (used by relogin, whose stored entry holds the token
// that just failed). With skipStore, a replacement that cannot be saved also
// drops the stale entry, so a restart cannot reload the failed token.
//
// A store Load error other than ErrSessionNotFound means the stored state is
// unknown. Provided credentials are then used as-is (nothing is saved); with
// none, the account fails rather than falling through to a password login,
// so a flaky store cannot trigger a login storm against x.com.
func (c *Client) establishCredentials(acc *Account, client *stealth.BrowserClient, skipStore bool) error {
	if acc.credsAuthoritative && acc.AuthToken != "" && acc.CT0 != "" {
		acc.ct0RefreshedAt = time.Now()
		slog.Info("using leased credentials", slog.String("user", acc.Username))
		return nil
	}

	var authToken, ct0 string
	if !skipStore {
		var loadErr error
		authToken, ct0, loadErr = c.loadSession(acc)
		if loadErr != nil {
			if acc.AuthToken != "" && acc.CT0 != "" {
				acc.ct0RefreshedAt = time.Now()
				slog.Info("using provided credentials (session store unavailable)", slog.String("user", acc.Username))
				return nil
			}
			return fmt.Errorf("session store unavailable for %s, not logging in: %w", acc.Username, loadErr)
		}
	}
	if authToken != "" && ct0 != "" {
		acc.AuthToken = authToken
		acc.CT0 = ct0
		acc.ct0RefreshedAt = time.Now()
		slog.Info(c.sessionLoadedMsg(), slog.String("user", acc.Username), slog.String("sample_key", "session_load"))
		return nil
	}

	if acc.AuthToken != "" && acc.CT0 != "" {
		acc.ct0RefreshedAt = time.Now()
		slog.Info("using provided credentials", slog.String("user", acc.Username))
		c.saveSession(acc, acc.AuthToken, acc.CT0)
		return nil
	}

	if acc.Password == "" {
		return fmt.Errorf("no session and no password for account %s", acc.Username)
	}

	if err := c.login(acc, client); err != nil {
		return fmt.Errorf("login failed for %s: %w", acc.Username, err)
	}

	if err := c.saveSession(acc, acc.AuthToken, acc.CT0); err != nil && skipStore {
		c.dropStaleSession(acc, err)
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
	g := accountlessWire(c.cfg)
	bc, err := stealth.NewClient(append([]stealth.ClientOption{
		stealth.WithHeaderOrder(twitterHeaderOrder),
	}, wireClientOptions(g)...)...)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	c.owned.setWire(bc, g)
	defer c.owned.forgetWire(bc)
	// The client serves this login only. Release its idle connections when the
	// login ends rather than listing it on the Client: listing would pin its
	// jar, which holds the open account's cookies, for the Client's lifetime.
	defer bc.CloseIdleConnections()
	return c.openAccountOn(bc)
}

// openAccountOn runs the welcome flow on bc and returns the account it minted.
func (c *Client) openAccountOn(bc *stealth.BrowserClient) (*Account, error) {
	guestToken, err := c.getGuestToken(bc)
	if err != nil {
		return nil, fmt.Errorf("guest token: %w", err)
	}

	headers := loginFlowHeaders(guestToken, "", clientUA(bc))
	welcomeURL := twitterAPIURL + "/1.1/onboarding/task.json?flow_name=welcome"
	body, _, status, err := bc.DoWithHeaderOrder("POST", welcomeURL,
		headers, strings.NewReader(openAccountPayload), c.wireOrderFor(nil, bc, "POST", welcomeURL, headers),
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
			stepURL := twitterAPIURL + "/1.1/onboarding/task.json"
			body2, _, status2, err := bc.DoWithHeaderOrder("POST", stepURL,
				headers, strings.NewReader(payload), c.wireOrderFor(nil, bc, "POST", stepURL, headers),
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
	// The session was minted under bc's browser identity; the account keeps it
	// instead of deriving another from its username on the first request.
	id := bc.Identity()
	return &Account{
		Username:  username,
		AuthToken: authToken,
		CT0:       ct0,
		Profile:   id.BrowserProfile,
		UserAgent: id.UserAgent,
		active:    true,
	}, nil
}

// getGuestToken fetches a Twitter guest token.
func (c *Client) getGuestToken(client *stealth.BrowserClient) (string, error) {
	ua := clientUA(client)
	headers := withClientHints(map[string]string{
		"authorization": "Bearer " + BearerToken,
		"content-type":  "application/json",
		"user-agent":    ua,
	}, ua)
	activateURL := twitterAPIURL + "/1.1/guest/activate.json"
	body, _, status, err := client.DoWithHeaderOrder("POST", activateURL, headers, nil, c.wireOrderFor(nil, client, "POST", activateURL, headers))
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

// guestCircuitBreakerThreshold is the number of consecutive failures before blocking guest tokens.
const guestCircuitBreakerThreshold = 5

// guestCircuitBreakerWindow is how long the circuit breaker stays open.
const guestCircuitBreakerWindow = 30 * time.Minute

// acquireGuestToken fetches a fresh guest token with exponential backoff.
// On success, resets the consecutive failure counter. On exhaustion, increments
// the counter and trips the circuit breaker when the threshold is reached.
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
			c.mu.Lock()
			c.guestConsecFails = 0
			c.mu.Unlock()
			return token, nil
		}
		lastErr = err
		slog.Warn("guest token acquisition failed", slog.Int("attempt", attempt+1), slog.Any("error", err))
	}

	c.mu.Lock()
	c.guestConsecFails++
	fails := c.guestConsecFails
	if fails >= guestCircuitBreakerThreshold {
		c.guestBlockedUntil = time.Now().Add(guestCircuitBreakerWindow)
		c.guestConsecFails = 0
		slog.Warn("guest token circuit breaker tripped",
			slog.Int("consec_fails", fails),
			slog.Duration("blocked_for", guestCircuitBreakerWindow))
	}
	c.mu.Unlock()

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
	headers := loginFlowHeaders(guestToken, "", clientUA(client))
	payload := `{"input_flow_data":{"flow_context":{"debug_overrides":{},"start_location":{"location":"splash_screen"}}},"subtask_versions":{"action_list":2,"alert_dialog":1,"app_download_cta":1,"check_logged_in_account":1,"choice_selection":3,"contacts_live_sync_permission_prompt":0,"cta":7,"email_verification":2,"end_flow":1,"enter_date":1,"enter_email":2,"enter_password":5,"enter_phone":2,"enter_recaptcha":1,"enter_text":5,"enter_username":2,"generic_urt":3,"in_app_notification":1,"interest_picker":3,"js_instrumentation":1,"menu_dialog":1,"notifications_permission_prompt":2,"open_account":2,"open_home_timeline":1,"open_link":1,"phone_verification":4,"privacy_options":1,"security_key":3,"select_avatar":4,"select_banner":2,"settings_list":7,"show_code":1,"sign_up":2,"sign_up_review":4,"tweet_selection_urt":1,"update_users":1,"upload_media":1,"user_recommendations_list":4,"user_recommendations_urt":1,"wait_spinner":3,"web_modal":1}}`

	initURL := twitterAPIURL + "/1.1/onboarding/task.json?flow_name=login"
	body, _, status, err := client.DoWithHeaderOrder("POST", initURL,
		headers,
		strings.NewReader(payload),
		c.wireOrderFor(nil, client, "POST", initURL, headers),
	)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("init flow: HTTP %d: %s", status, string(body[:min(300, len(body))]))
	}
	return parseFlowResponse(body)
}

func (c *Client) submitFlowStep(client *stealth.BrowserClient, guestToken, payload string) (*flowResponse, error) {
	headers := loginFlowHeaders(guestToken, "", clientUA(client))
	stepURL := twitterAPIURL + "/1.1/onboarding/task.json"
	body, _, status, err := client.DoWithHeaderOrder("POST", stepURL,
		headers,
		strings.NewReader(payload),
		c.wireOrderFor(nil, client, "POST", stepURL, headers),
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
