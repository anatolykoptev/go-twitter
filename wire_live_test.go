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
			Headers  []string `json:"headers"`
			Priority struct {
				Weight int `json:"weight"`
			} `json:"priority"`
		} `json:"sent_frames"`
	} `json:"http2"`
}

// seen is what the echo service reports of the request: header names in wire
// order (pseudo-headers dropped, split cookie lines folded), their values, and
// the HTTP/2 HEADERS priority weight.
type seen struct {
	names  []string
	values map[string]string
	weight int
}

func parseSeen(t *testing.T, body []byte) seen {
	t.Helper()
	var r echoReply
	require.NoError(t, json.Unmarshal(body, &r), "echo service answer")
	for _, f := range r.HTTP2.Frames {
		if len(f.Headers) == 0 {
			continue
		}
		s := seen{values: map[string]string{}, weight: f.Priority.Weight}
		for _, line := range f.Headers {
			if strings.HasPrefix(line, ":") {
				continue
			}
			name, val, _ := strings.Cut(line, ": ")
			if len(s.names) > 0 && s.names[len(s.names)-1] == name && name == "cookie" {
				continue
			}
			s.names = append(s.names, name)
			s.values[name] = val
		}
		return s
	}
	t.Fatal("echo service reported no request headers")
	return seen{}
}

func liveClient(t *testing.T, p stealth.BrowserProfile) *stealth.BrowserClient {
	t.Helper()
	// Built the way the Client builds an account's client of this profile.
	bc, err := stealth.NewClient(append([]stealth.ClientOption{
		stealth.WithIdentity(stealth.BrowserIdentity{BrowserProfile: p}), stealth.WithHeaderOrder(twitterHeaderOrder),
	}, wireClientOptions(wireGenFor(p.TLSProfile))...)...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

// send shapes h as a request to shapeURL and sends it to the echo service.
func send(t *testing.T, bc *stealth.BrowserClient, gen wireGen, method, shapeURL string, h map[string]string, body string) seen {
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
	return parseSeen(t, resp)
}

func TestLive_CurrentIdentityMatchesChrome(t *testing.T) {
	p := chromeProfile(t, stealth.ProfileChrome156, "chrome", "macos")
	bc := liveClient(t, p)

	h := twitterHeaders("dummy", "dummy", p.UserAgent)
	h["x-client-transaction-id"] = "dummy"
	got := send(t, bc, wireChrome, "GET", "https://x.com/i/api/graphql/q/Op", h, "")
	t.Logf("authenticated same-origin GET: weight=%d %v", got.weight, got.names)
	// x-xp-forwarded-for has no captured position and is not sent here.
	assert.Equal(t, oracleSameOriginAuthedGet, got.names)
	assert.Equal(t, 220, got.weight, "XHR HEADERS weight")
	assert.Equal(t, "u=1, i", got.values["priority"])
	assert.Equal(t, "gzip, deflate, br, zstd", got.values["accept-encoding"])

	h = guestHeaders("dummy", p.UserAgent)
	h["x-client-transaction-id"] = "dummy"
	h["cookie"] = "guest_id=dummy"
	delete(h, "content-type")
	got = send(t, bc, wireChrome, "GET", "https://api.x.com/1.1/hashflags.json", h, "")
	t.Logf("logged-out XHR GET: weight=%d %v", got.weight, got.names)
	assert.Equal(t, oracleXHRGet, got.names)
	assert.Equal(t, 220, got.weight)

	h = guestHeaders("dummy", p.UserAgent)
	h["x-client-transaction-id"] = "dummy"
	h["cookie"] = "guest_id=dummy"
	got = send(t, bc, wireChrome, "POST", "https://api.x.com/1.1/flow/viewer.json", h, "{}")
	t.Logf("logged-out XHR POST: weight=%d %v", got.weight, got.names)
	assert.Equal(t, oracleXHRPost, got.names)
	assert.Equal(t, 220, got.weight)
}

func TestLive_LegacyIdentityMatchesV086(t *testing.T) {
	bc := liveClient(t, legacyProfile)
	ua := legacyProfile.UserAgent

	h := twitterHeaders("dummy", "dummy", ua)
	h["x-client-transaction-id"] = "dummy"
	h["x-xp-forwarded-for"] = "dummy"
	got := send(t, bc, wireLegacy, "GET", "https://x.com/i/api/graphql/q/Op", h, "")
	t.Logf("legacy authenticated GET: weight=%d %v", got.weight, got.names)
	assert.Equal(t, strings.Split(wireAuthGET, ","), got.names)
	assert.Equal(t, 256, got.weight, "legacy keeps the navigation weight")
	assert.NotContains(t, got.values, "priority")
	assert.Equal(t, "gzip, deflate, br", got.values["accept-encoding"])

	got = send(t, bc, wireLegacy, "POST", apiFlowInit, loginFlowHeaders("dummy", "", ua), "{}")
	t.Logf("legacy login-flow POST: weight=%d %v", got.weight, got.names)
	assert.Equal(t, strings.Split(wireFlowPOST, ","), got.names)
	assert.Equal(t, 256, got.weight)
	assert.NotContains(t, got.values, "priority")
}
