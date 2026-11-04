//go:build livewire

package twitter

import (
	"encoding/json"
	"strings"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Opt-in live check (go test -tags livewire -run Live -v .): sends the shapes
// this package builds, with dummy credentials, through the real TLS transport to
// an echo service and compares the header order the service saw with the
// browser capture (current identity) and with v0.8.6 (legacy identity). An
// unreachable service FAILS; it is never skipped. Only the echo service is
// contacted, never x.com.
const echoURL = "https://tls.peet.ws/api/all"

type echoReply struct {
	HTTP2 struct {
		Frames []struct {
			Headers []string `json:"headers"`
		} `json:"sent_frames"`
	} `json:"http2"`
}

// seenOrder is the header-name order the echo service reports, without
// pseudo-headers and with the split cookie lines folded into one.
func seenOrder(t *testing.T, body []byte) []string {
	t.Helper()
	var r echoReply
	require.NoError(t, json.Unmarshal(body, &r), "echo service answer")
	for _, f := range r.HTTP2.Frames {
		if len(f.Headers) == 0 {
			continue
		}
		var out []string
		for _, line := range f.Headers {
			name, _, _ := strings.Cut(line, ":")
			if name == "" || strings.HasPrefix(line, ":") {
				continue
			}
			if len(out) > 0 && out[len(out)-1] == name && name == "cookie" {
				continue
			}
			out = append(out, name)
		}
		return out
	}
	t.Fatal("echo service reported no request headers")
	return nil
}

func liveClient(t *testing.T, p stealth.BrowserProfile) *stealth.BrowserClient {
	t.Helper()
	bc, err := stealth.NewClient(stealth.WithIdentity(stealth.BrowserIdentity{BrowserProfile: p}), stealth.WithHeaderOrder(twitterHeaderOrder))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

// send shapes h as a request to shapeURL and sends it to the echo service.
func send(t *testing.T, bc *stealth.BrowserClient, gen wireGen, method, shapeURL string, h map[string]string, body string) []string {
	t.Helper()
	order := gen.shape(method, shapeURL, h)
	var rd *strings.Reader
	var resp []byte
	var status int
	var err error
	if body != "" {
		rd = strings.NewReader(body)
		resp, _, status, err = bc.DoWithHeaderOrder(method, echoURL, h, rd, order)
	} else {
		resp, _, status, err = bc.DoWithHeaderOrder(method, echoURL, h, nil, order)
	}
	require.NoError(t, err, "echo service unreachable")
	require.Equal(t, 200, status)
	return seenOrder(t, resp)
}

func TestLive_CurrentIdentityMatchesChrome(t *testing.T) {
	p := chromeProfile(t, stealth.ProfileChrome152, "chrome", "macos")
	bc := liveClient(t, p)

	h := twitterHeaders("dummy", "dummy", p.UserAgent)
	h["x-client-transaction-id"] = "dummy"
	got := send(t, bc, wireChrome, "GET", "https://x.com/i/api/graphql/q/Op", h, "")
	t.Logf("authenticated same-origin GET: %v", got)
	// x-xp-forwarded-for has no captured position and is not sent here.
	assert.Equal(t, without(oracleSameOriginAuthedGet, "priority"), got)

	h = guestHeaders("dummy", p.UserAgent)
	h["x-client-transaction-id"] = "dummy"
	h["cookie"] = "guest_id=dummy"
	delete(h, "content-type")
	got = send(t, bc, wireChrome, "GET", "https://api.x.com/1.1/hashflags.json", h, "")
	t.Logf("logged-out XHR GET: %v", got)
	assert.Equal(t, without(oracleXHRGet, "priority"), got)

	h = guestHeaders("dummy", p.UserAgent)
	h["x-client-transaction-id"] = "dummy"
	h["cookie"] = "guest_id=dummy"
	got = send(t, bc, wireChrome, "POST", "https://api.x.com/1.1/flow/viewer.json", h, "{}")
	t.Logf("logged-out XHR POST: %v", got)
	assert.Equal(t, without(oracleXHRPost, "priority"), got)
}

func TestLive_LegacyIdentityMatchesV086(t *testing.T) {
	bc := liveClient(t, legacyProfile)
	ua := legacyProfile.UserAgent

	h := twitterHeaders("dummy", "dummy", ua)
	h["x-client-transaction-id"] = "dummy"
	h["x-xp-forwarded-for"] = "dummy"
	got := send(t, bc, wireLegacy, "GET", "https://x.com/i/api/graphql/q/Op", h, "")
	t.Logf("legacy authenticated GET: %v", got)
	assert.Equal(t, strings.Split(wireAuthGET, ","), got)

	got = send(t, bc, wireLegacy, "POST", apiFlowInit, loginFlowHeaders("dummy", "", ua), "{}")
	t.Logf("legacy login-flow POST: %v", got)
	assert.Equal(t, strings.Split(wireFlowPOST, ","), got)
}
