package twitter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-twitter/xpff"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// production407 is the verbatim error string a dead HTTP proxy produces in
// production (tls-client connect.go:229/269): "Proxy" is capitalised twice and
// no lowercase "proxy" appears, so the pre-fix isProxyError returned false.
const production407 = `Get "https://x.com/i/api/graphql/FOlovQsiHGDls3c0Q_HaSQ/UserTweets?variables=...": Proxy responded with non 200 code: 407 Proxy Authentication Required`

// newProxyAttributionClient builds a minimal Client (no stealth stack) with a
// capturing alertHook, exactly like newSelfHealTestClient but wired for the
// proxy-attribution path under test.
func newProxyAttributionClient(t *testing.T) (*Client, *Account, *[]proxyDownAlert) {
	t.Helper()
	acc := &Account{Username: "proxy_user", Proxy: "http://dead-proxy:8080"}
	acc.active = true
	acc.HealthTracker = pool.DefaultHealthTracker()
	var alerts []proxyDownAlert
	hook := func(topic string, payload any) {
		alerts = append(alerts, proxyDownAlert{topic: topic, payload: payload})
	}
	c := &Client{
		pool:      pool.New([]*Account{acc}, pool.DefaultConfig()),
		cfg:       ClientConfig{ProxyBackoffInitial: 30 * time.Second, ProxyBackoffMax: 30 * time.Minute},
		alertHook: hook,
	}
	return c, acc, &alerts
}

type proxyDownAlert struct {
	topic   string
	payload any
}

// --- Requirement 1: the verbatim production 407 is classified as a proxy error ---

// TestIsProxyError_Production407 — the exact production error string must be
// classified as a proxy error.
// Mutation: revert isProxyError to lowercase-only `strings.Contains(msg, "proxy")`
// => RED (capital "Proxy" no longer matches).
func TestIsProxyError_Production407(t *testing.T) {
	if !isProxyError(errors.New(production407)) {
		t.Fatal("production 407 error must be classified as a proxy error; isProxyError returned false")
	}
}

// TestIsProxyError_LowercaseNoRegression — a lowercase "proxy" error still
// classifies (no regression in the case-insensitive widening).
func TestIsProxyError_LowercaseNoRegression(t *testing.T) {
	cases := []string{
		"proxyconnect tcp: dial tcp: connection refused",
		"socks5 connect: connection refused",
		"tunnel connection failed",
		"connection refused",
		"no such host",
	}
	for _, msg := range cases {
		if !isProxyError(errors.New(msg)) {
			t.Fatalf("lowercase transport error must still classify as proxy: %q", msg)
		}
	}
}

// --- Requirement 2: counter-test — an X-side auth error is NOT a proxy error ---

// TestIsProxyError_XSideAuthErrorNotProxy — an X-side authentication failure
// (code 32 / 401 body, the shape this client surfaces as *APIError) must NOT be
// classified as a proxy error.
// Mutation: make isProxyError return true unconditionally => RED.
func TestIsProxyError_XSideAuthErrorNotProxy(t *testing.T) {
	xSideErr := &APIError{
		Status:  401,
		Class:   errAuthExpired,
		Codes:   []int{32},
		Message: `UserTweets HTTP 401: {"errors":[{"code":32,"message":"Could not authenticate you"}]}`,
	}
	if isProxyError(xSideErr) {
		t.Fatal("X-side auth error (code 32) must NOT be classified as a proxy error")
	}
}

// TestIsProxyError_XSide403BodyNotProxy — a 403 body must not match on a
// coincidental "proxy" substring in a URL or body.
func TestIsProxyError_XSide403BodyNotProxy(t *testing.T) {
	xSideErr := &APIError{
		Status:  403,
		Class:   errForbidden,
		Message: `SearchTimeline HTTP 403: {"errors":[{"code":200,"message":"Forbidden"}]}`,
	}
	if isProxyError(xSideErr) {
		t.Fatal("X-side 403 forbidden must NOT be classified as a proxy error")
	}
}

// --- Requirement 1 seam: markProxyDown fires, RecordFailure does NOT ---

// TestAttributeTransportError_ProxyErrorMarksProxyDownNotAccount — at the
// request.go:86 seam, a proxy error causes markProxyDown to fire and
// RecordFailure NOT to fire.
// Mutation: swap the branches (call RecordFailure for proxy errors) => RED
// (proxyConsecFails stays 0, account failed climbs).
func TestAttributeTransportError_ProxyErrorMarksProxyDownNotAccount(t *testing.T) {
	c, acc, alerts := newProxyAttributionClient(t)
	c.attributeTransportError(acc, errors.New(production407))

	acc.mu.Lock()
	gotConsec := acc.proxyConsecFails
	gotBackoff := acc.proxyBackoff
	acc.mu.Unlock()
	if gotConsec != 1 {
		t.Fatalf("proxy error must increment proxyConsecFails: got %d, want 1", gotConsec)
	}
	if !gotBackoff.After(time.Now()) {
		t.Fatalf("proxy error must set a future proxyBackoff: got %v", gotBackoff)
	}
	total, failed, consec := acc.Stats()
	if failed != 0 || consec != 0 {
		t.Fatalf("proxy error must NOT charge the account: total=%d failed=%d consec=%d", total, failed, consec)
	}
	if len(*alerts) != 1 || (*alerts)[0].topic != "proxy.down" {
		t.Fatalf("proxy error must emit a proxy.down alert: got %v", *alerts)
	}
}

// TestAttributeTransportError_NonProxyErrorRecordsAccountFailure — a non-proxy
// transport error charges the account, not the proxy.
func TestAttributeTransportError_NonProxyErrorRecordsAccountFailure(t *testing.T) {
	c, acc, alerts := newProxyAttributionClient(t)
	c.attributeTransportError(acc, errors.New("i/o timeout"))

	acc.mu.Lock()
	gotConsec := acc.proxyConsecFails
	acc.mu.Unlock()
	if gotConsec != 0 {
		t.Fatalf("non-proxy error must NOT increment proxyConsecFails: got %d", gotConsec)
	}
	_, failed, consec := acc.Stats()
	if failed != 1 || consec != 1 {
		t.Fatalf("non-proxy error must charge the account: failed=%d consec=%d", failed, consec)
	}
	if len(*alerts) != 0 {
		t.Fatalf("non-proxy error must NOT emit a proxy.down alert: got %v", *alerts)
	}
}

// TestAttributeTransportError_NoProxyRecordsAccountFailure — a proxy error on an
// account with NO proxy configured charges the account (the acc.Proxy != ""
// guard).
func TestAttributeTransportError_NoProxyRecordsAccountFailure(t *testing.T) {
	c, acc, alerts := newProxyAttributionClient(t)
	acc.Proxy = "" // no proxy configured
	c.attributeTransportError(acc, errors.New(production407))

	acc.mu.Lock()
	gotConsec := acc.proxyConsecFails
	acc.mu.Unlock()
	if gotConsec != 0 {
		t.Fatalf("no-proxy account must NOT increment proxyConsecFails: got %d", gotConsec)
	}
	_, failed, _ := acc.Stats()
	if failed != 1 {
		t.Fatalf("no-proxy account must charge the account on a transport error: failed=%d", failed)
	}
	if len(*alerts) != 0 {
		t.Fatalf("no-proxy account must NOT emit proxy.down: got %v", *alerts)
	}
}

// --- Requirement 3: observability — alert hook + HealthReport ---

// TestMarkProxyDown_EmitsAlertHook — markProxyDown emits a "proxy.down" alert
// through the PoolAlertHook seam with a payload a consumer can read.
// Mutation: remove the alertHook call in markProxyDown => RED.
func TestMarkProxyDown_EmitsAlertHook(t *testing.T) {
	c, acc, alerts := newProxyAttributionClient(t)
	c.markProxyDown(acc)

	require.Len(t, *alerts, 1, "markProxyDown must emit exactly one alert")
	a := (*alerts)[0]
	assert.Equal(t, "proxy.down", a.topic)
	m, ok := a.payload.(map[string]any)
	require.True(t, ok, "payload must be map[string]any")
	assert.Equal(t, "proxy_user", m["user"])
	assert.NotEmpty(t, m["proxy"], "payload must carry the masked proxy")
	assert.Equal(t, 1, m["consec_fails"])
	bd, ok := m["backoff"].(time.Duration)
	require.True(t, ok, "payload backoff must be time.Duration")
	assert.Positive(t, bd)
}

// TestHealthReport_SurfacesProxyState — HealthReport distinguishes proxy-layer
// from account-layer failure by surfacing proxyConsecFails and proxyBackoff.
// Mutation: don't populate ProxyConsecFails/ProxyBackoffUntil in HealthReport => RED.
func TestHealthReport_SurfacesProxyState(t *testing.T) {
	c, acc, _ := newProxyAttributionClient(t)
	c.markProxyDown(acc)
	c.markProxyDown(acc)

	report := c.HealthReport()
	require.Len(t, report, 1)
	h := report[0]
	assert.Equal(t, "proxy_user", h.Username)
	assert.Equal(t, 2, h.ProxyConsecFails, "HealthReport must surface proxyConsecFails")
	assert.True(t, h.ProxyBackoffUntil.After(time.Now()), "HealthReport must surface a future proxyBackoffUntil")
	// Account-layer stats must be clean (the whole point of correct attribution).
	assert.Equal(t, 0, h.Failed, "account failed must be 0 when only the proxy is down")
	assert.Equal(t, 0, h.ConsecFails, "account consec must be 0 when only the proxy is down")
}

// TestHealthReport_AccountFailureNotProxy — a plain account failure does NOT
// inflate proxy state (the distinction runs both ways).
func TestHealthReport_AccountFailureNotProxy(t *testing.T) {
	c, acc, _ := newProxyAttributionClient(t)
	acc.RecordFailure()

	report := c.HealthReport()
	require.Len(t, report, 1)
	h := report[0]
	assert.Equal(t, 0, h.ProxyConsecFails, "account failure must not inflate proxyConsecFails")
	assert.True(t, h.ProxyBackoffUntil.IsZero(), "account failure must not set proxyBackoffUntil")
	assert.Equal(t, 1, h.Failed)
}

// --- Requirement 1 structural-signal reachability (documented by test) ---

// TestIsProxyError_StructuralSignalNotReachable documents that the proxy 407 is
// a plain errors.errorString from tls-client (connect.go:229/269) with no typed
// wrapper or status-code field, so a structural classification is NOT reachable
// from the error the transport returns — string matching is the only option.
func TestIsProxyError_StructuralSignalNotReachable(t *testing.T) {
	// tls-client returns errors.New("Proxy responded with non 200 code: " + resp.Status).
	// That is an unexportd *errors.errorString: no Unwrap, no typed field, no status.
	err := errors.New(production407)
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatal("transport proxy error must NOT be an *APIError (structural signal unreachable)")
	}
	// The status code (407) lives only inside the prose — confirm it is not a
	// separate, extractable value.
	assert.NotContains(t, fmt.Sprintf("%T", err), "tlsclient", "no tls-client typed error reaches go-twitter")
}

// --- URL false positive: user-supplied content in the URL must not match ---

// searchTimelineProxyURL simulates a transport error on a SearchTimeline request
// whose rawQuery is "proxy" — the word "proxy" appears ONLY in the request URL,
// not in the transport's own message. Without URL stripping, isProxyError would
// match on the URL and mis-attribute a non-proxy transport error to the proxy.
const searchTimelineProxyURL = `Get "https://x.com/i/api/graphql/GcXk9vN_d1jUfHNqLacXQA/SearchTimeline?variables=%7B%22rawQuery%22%3A%22proxy%22%7D": i/o timeout`

// userByScreenNameProxyHandleURL simulates a transport error on a
// UserByScreenName request whose screenName is "proxyhandle" — the word "proxy"
// appears ONLY in the request URL.
const userByScreenNameProxyHandleURL = `Get "https://x.com/i/api/graphql/IGgvgiOx4QZndDHuD3x9TQ/UserByScreenName?variables=%7B%22screen_name%22%3A%22proxyhandle%22%7D": dial tcp: i/o timeout`

// TestIsProxyError_URLContainingProxyNotProxy — a transport error whose request
// URL contains "proxy" (from user-supplied rawQuery or screenName) but whose
// transport message is NOT a proxy failure must NOT classify as a proxy error.
// Mutation: revert isProxyError to match on the whole string (remove the
// quotedURLRe strip) => RED ("proxy" in the URL matches).
func TestIsProxyError_URLContainingProxyNotProxy(t *testing.T) {
	cases := []string{
		searchTimelineProxyURL,
		userByScreenNameProxyHandleURL,
	}
	for _, msg := range cases {
		if isProxyError(errors.New(msg)) {
			t.Fatalf("transport error with 'proxy' only in the URL must NOT be classified as a proxy error: %q", msg)
		}
	}
}

// TestAttributeTransportError_URLProxyWordChargesAccount — at the
// attributeTransportError seam, a transport error whose URL contains "proxy"
// on an account WITH a proxy configured must charge the ACCOUNT, not the proxy.
// Mutation: revert isProxyError to match on the whole string (remove the
// quotedURLRe strip) => RED (proxyConsecFails increments instead of failed).
func TestAttributeTransportError_URLProxyWordChargesAccount(t *testing.T) {
	c, acc, alerts := newProxyAttributionClient(t)
	c.attributeTransportError(acc, errors.New(searchTimelineProxyURL))

	acc.mu.Lock()
	gotConsec := acc.proxyConsecFails
	acc.mu.Unlock()
	if gotConsec != 0 {
		t.Fatalf("transport error with 'proxy' in URL must NOT increment proxyConsecFails: got %d, want 0", gotConsec)
	}
	_, failed, consec := acc.Stats()
	if failed != 1 || consec != 1 {
		t.Fatalf("transport error with 'proxy' in URL must charge the account: failed=%d consec=%d, want 1/1", failed, consec)
	}
	if len(*alerts) != 0 {
		t.Fatalf("transport error with 'proxy' in URL must NOT emit proxy.down: got %v", *alerts)
	}
}

// --- Transport-coupled fidelity: real transport error through the actual seam ---

// failingFetcher is a bundle.Fetcher that always returns an error. Used to wire
// xtidMgr in tests without a live network — GenerateID degrades gracefully
// (returns an error, doRequestWithBody logs debug and skips the header).
type failingFetcher struct{}

func (f *failingFetcher) Fetch(_ context.Context, _ string) ([]byte, error) {
	return nil, errors.New("test fetcher: offline")
}

// newTransportSeamClient builds a Client wired with a real stealth.BrowserClient
// (dead proxy at 127.0.0.1:1), xtidMgr (failing fetcher), and xpffGen so that
// doRequestWithBody runs the full xtid→xpff→bc.DoWithHeaderOrder path and
// produces a genuine transport error (connection refused) through the real seam.
func newTransportSeamClient(t *testing.T) (*Client, *Account, *[]proxyDownAlert) {
	t.Helper()
	acc := &Account{Username: "seam_user", Proxy: "http://127.0.0.1:1"}
	acc.active = true
	acc.HealthTracker = pool.DefaultHealthTracker()

	bc, err := stealth.NewClient(stealth.WithoutSSRFGuard(), stealth.WithProxy("http://127.0.0.1:1"))
	require.NoError(t, err, "stealth client with dead proxy")

	var alerts []proxyDownAlert
	hook := func(topic string, payload any) {
		alerts = append(alerts, proxyDownAlert{topic: topic, payload: payload})
	}
	c := &Client{
		client:    bc,
		pool:      pool.New([]*Account{acc}, pool.DefaultConfig()),
		xtidMgr:   xtid.NewManager(&failingFetcher{}),
		xpffGen:   xpff.New("test-guest-id", defaultUserAgent),
		cfg:       ClientConfig{ProxyBackoffInitial: 30 * time.Second, ProxyBackoffMax: 30 * time.Minute, DisableGuestFallback: true},
		alertHook: hook,
	}
	return c, acc, &alerts
}

// TestDoPoolRequest_RealTransportErrorAttributedToProxy drives a REAL transport
// error (connection refused to a dead proxy at 127.0.0.1:1) through the actual
// doPoolRequest → doPoolReq → doRequestWithBody → bc.DoWithHeaderOrder seam —
// not a call to attributeTransportError directly. The error is genuine: the
// stealth BrowserClient dials the dead proxy, tls-client returns a transport
// error, and it flows back through doRequestWithBody (which injects xtid/xpff
// headers) to doPoolRequest's err != nil branch, which calls
// attributeTransportError → markProxyDown.
//
// Mutation: make attributeTransportError a no-op (return without calling
// markProxyDown or RecordFailure) => RED (proxyConsecFails stays 0, no alert).
func TestDoPoolRequest_RealTransportErrorAttributedToProxy(t *testing.T) {
	c, acc, alerts := newTransportSeamClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use a non-auth endpoint so pool.Next (not NextWithWait) is used — avoids
	// a 5-minute wait after the proxy is marked down and the account is filtered.
	_, _, err := c.doPoolRequest(ctx, "GET", "TestEndpoint", "https://x.com/i/api/graphql/test/TestOp", nil)
	require.Error(t, err, "doPoolRequest must return an error when the proxy is dead")

	acc.mu.Lock()
	gotConsec := acc.proxyConsecFails
	gotBackoff := acc.proxyBackoff
	acc.mu.Unlock()
	assert.Equal(t, 1, gotConsec, "real transport error through the seam must increment proxyConsecFails")
	assert.True(t, gotBackoff.After(time.Now()), "real transport error through the seam must set a future proxyBackoff")

	total, failed, consec := acc.Stats()
	assert.Equal(t, 0, failed, "proxy transport error must NOT charge the account: failed=%d", failed)
	assert.Equal(t, 0, consec, "proxy transport error must NOT charge the account: consec=%d", consec)
	_ = total

	require.Len(t, *alerts, 1, "real transport error through the seam must emit one proxy.down alert")
	assert.Equal(t, "proxy.down", (*alerts)[0].topic)
}
