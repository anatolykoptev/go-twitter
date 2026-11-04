package twitter

import (
	"context"
	"strings"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Oracle: Google Chrome 156.0.8078.80 (macOS, headful) navigating to
// https://x.com/NASA logged out (its network log, xcom_gchrome156_default): wire
// order after the pseudo-headers, HTTP/2 HEADERS weight 256. The cookie sits
// where the same browser puts it on subresources.
var oracleNavigationGet = strings.Split("sec-ch-ua,sec-ch-ua-mobile,sec-ch-ua-platform,upgrade-insecure-requests,"+
	"user-agent,accept,sec-fetch-site,sec-fetch-mode,sec-fetch-user,sec-fetch-dest,accept-encoding,"+
	"accept-language,cookie,priority", ",")

const oracleNavigationAccept156 = "text/html,application/xhtml+xml,application/xml;q=0.9,image/jxl,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"

func warmAccountClient(t *testing.T, acc *Account) (*Client, *wireRecorder) {
	t.Helper()
	rec := &wireRecorder{}
	c, err := NewClient(ClientConfig{
		Accounts: []*Account{acc}, DefaultProxy: deadProxy,
		stealthOpts: []stealth.ClientOption{stealth.WithBackend(rec.factory)},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, rec
}

// The page a current identity's warm fetch loads is a document navigation: the
// navigation header set and order of the oracle, from a client built without
// fetch mode (HTTP/2 weight 256), carrying the account's cookie.
//
// Mutation: client.go warmClient — return bc instead of the warmRouter => RED
// (priority u=1, i, no sec-fetch-dest document, wrong order).
func TestWarmNavigation_PageIsADocumentNavigation(t *testing.T) {
	acc := &Account{Username: "nav", AuthToken: "t", CT0: "c", Profile: chromeProfile(t, stealth.ProfileChrome156, "chrome", "macos")}
	c, rec := warmAccountClient(t, acc)

	d := c.warmClient(acc, acc.client)
	_, _, _, _ = d.DoCtx(context.Background(), "GET", "https://x.com/home", map[string]string{
		"User-Agent": "generic", "Accept": "text/html", "Accept-Language": "xx", "cookie": "auth_token=t; ct0=c",
	}, nil)
	got := rec.last()
	assert.Equal(t, oracleNavigationGet, wireNames(got))
	assert.Equal(t, "u=0, i", got.Headers["priority"])
	assert.Equal(t, "document", got.Headers["sec-fetch-dest"])
	assert.Equal(t, "navigate", got.Headers["sec-fetch-mode"])
	assert.Equal(t, "none", got.Headers["sec-fetch-site"])
	assert.Equal(t, "?1", got.Headers["sec-fetch-user"])
	assert.Equal(t, "1", got.Headers["upgrade-insecure-requests"])
	assert.Equal(t, oracleNavigationAccept156, got.Headers["accept"])
	assert.Equal(t, "en-US,en;q=0.9", got.Headers["accept-language"])
	assert.Equal(t, "auth_token=t; ct0=c", got.Headers["cookie"])
	assert.Equal(t, acc.UserAgent, got.Headers["user-agent"])
	assert.NotEmpty(t, got.Headers["sec-ch-ua"])
	assert.NotContains(t, got.Headers, "referer")

	// HTTP/2 weight is per client: the account's own client is fetch mode (220),
	// the navigation client is not (256).
	require.GreaterOrEqual(t, len(rec.cfgs), 3)
	assert.True(t, rec.cfgs[1].FetchMode, "the account client is built in fetch mode")
	assert.False(t, rec.cfgs[len(rec.cfgs)-1].FetchMode, "the navigation client is not")
}

// A CDN script is not a navigation: it stays on the account's own fetch-mode
// client, unchanged.
//
// Mutation: warm_nav.go isDocumentURL — return true for every host => RED.
func TestWarmNavigation_ScriptsStayOnTheAccountClient(t *testing.T) {
	acc := &Account{Username: "nav2", AuthToken: "t", CT0: "c", Profile: chromeProfile(t, stealth.ProfileChrome156, "chrome", "macos")}
	c, rec := warmAccountClient(t, acc)
	_, _, _, _ = c.warmClient(acc, acc.client).DoCtx(context.Background(), "GET",
		"https://abs.twimg.com/responsive-web/client-web/main.abc.js", map[string]string{"User-Agent": "u"}, nil)
	got := rec.last()
	assert.Equal(t, "u=1, i", got.Headers["priority"], "fetch-mode client")
	assert.NotContains(t, got.Headers, "sec-fetch-dest")
}

// A legacy identity's warm fetches stay on its own client, untouched.
//
// Mutation: client.go navClientFor — delete the accountWire != wireChrome return => RED.
func TestWarmNavigation_LegacyAccountKeepsItsOwnClient(t *testing.T) {
	acc := &Account{Username: "legacy", AuthToken: "t", CT0: "c", Profile: legacyProfile, UserAgent: legacyProfile.UserAgent}
	c, rec := warmAccountClient(t, acc)
	d := c.warmClient(acc, acc.client)
	assert.Same(t, acc.client, d, "legacy: the warm fetch goes through the account's own client")
	assert.Nil(t, acc.navClient)
	_ = rec
}

// The form-submission hop of a warm fetch (x.com/x/migrate) is a same-origin
// navigation with origin and referer.
func TestWarmNavigation_FormPost(t *testing.T) {
	acc := &Account{Username: "nav3", AuthToken: "t", CT0: "c", Profile: chromeProfile(t, stealth.ProfileChrome156, "chrome", "macos")}
	c, rec := warmAccountClient(t, acc)
	_, _, _, _ = c.warmClient(acc, acc.client).DoCtx(context.Background(), "POST", "https://x.com/x/migrate",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "cookie": "auth_token=t"}, strings.NewReader("a=b"))
	got := rec.last()
	assert.Equal(t, "same-origin", got.Headers["sec-fetch-site"])
	assert.Equal(t, "https://x.com", got.Headers["origin"])
	assert.Equal(t, "document", got.Headers["sec-fetch-dest"])
	assert.Equal(t, "application/x-www-form-urlencoded", got.Headers["content-type"])
}

// The account-less fallback of a client that opted into the current wire
// fetches its warm pages as navigations too.
//
// Mutation: client.go NewClient — build the fallback fetcher on bc instead of
// the warmRouter => RED.
func TestWarmNavigation_AccountlessFallbackWhenOptedIn(t *testing.T) {
	rec := &wireRecorder{}
	c, err := NewClient(ClientConfig{
		DefaultProxy: deadProxy, ChromeWireForAccountlessClients: true,
		stealthOpts: []stealth.ClientOption{stealth.WithBackend(rec.factory)},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	h := guestHeaders("GT", clientUA(c.client)) // no auth_token cookie: the unauthenticated fallback fetcher
	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, c.client, "GET", "https://x.com/i/api/graphql/w/Op", h, nil)
	var page *recordedReq
	rec.mu.Lock()
	for i := range rec.reqs {
		if rec.reqs[i].URL == "https://x.com/home" || rec.reqs[i].URL == "https://x.com/xdevelopers" {
			page = &rec.reqs[i]
			break
		}
	}
	rec.mu.Unlock()
	require.NotNil(t, page, "the fallback fetcher requested a warm page")
	assert.Equal(t, "u=0, i", page.Headers["priority"])
	assert.Equal(t, "document", page.Headers["sec-fetch-dest"])
}
