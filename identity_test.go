package twitter

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/xpff"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// xpffBaseKey duplicates xpff's key constant on purpose: the test decrypts what
// went over the wire with its own copy, so it does not trust the generator it
// is checking.
const xpffBaseKey = "0e6be1f1e21ffc33590b888fd4dc81b19713e570e805d4e5df80a493c9571a05"

// openXpff decrypts an x-xp-forwarded-for value for guestID and returns the
// JSON payload.
func openXpff(guestID, hexValue string) (string, error) {
	raw, err := hex.DecodeString(hexValue)
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(xpffBaseKey + guestID))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("short xpff value")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	return string(plain), err
}

// wireReq is one request the fake X received.
type wireReq struct {
	method, path, token string
	h                   http.Header
}

// startRecordingX starts a fake X that records every request with the
// auth_token of its sender.
func startRecordingX(t *testing.T) (record func() []wireReq) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []wireReq
	)
	_ = startFakeX(t, func(w http.ResponseWriter, req *http.Request) {
		tok := strings.TrimPrefix(authTokenCookieRe.FindString(req.Header.Get("Cookie")), "auth_token=")
		mu.Lock()
		seen = append(seen, wireReq{method: req.Method, path: req.URL.Path, token: tok, h: req.Header.Clone()})
		mu.Unlock()
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
	})
	return func() []wireReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]wireReq(nil), seen...)
	}
}

func byToken(reqs []wireReq, tok string) []wireReq {
	var out []wireReq
	for _, r := range reqs {
		if r.token == tok {
			out = append(out, r)
		}
	}
	return out
}

// I1 — an account's browser profile is a function of the account, not of its
// position in the account list: the same account gets the same profile whatever
// the pool order or size.
//
// Mutation: account.go AssignBrowserProfile — pick `BuiltinProfiles[idx%len(...)]`
// by pool index again => RED (alice differs between the two pools).
func TestAssignBrowserProfile_StableAcrossPoolOrderAndSize(t *testing.T) {
	small := ParseAccounts("alice:p,bob:p,carol:p")
	big := ParseAccounts("carol:p,dave:p,erin:p,frank:p,gina:p,alice:p")
	find := func(accs []*Account, name string) *Account {
		for _, a := range accs {
			if a.Username == name {
				return a
			}
		}
		t.Fatalf("account %s missing", name)
		return nil
	}
	for _, name := range []string{"alice", "carol"} {
		a, b := find(small, name), find(big, name)
		assert.Equal(t, a.Profile, b.Profile, "%s: profile must not depend on pool position", name)
		assert.Equal(t, a.UserAgent, b.UserAgent, "%s: UA must not depend on pool position", name)
		assert.NotEmpty(t, a.UserAgent)
		assert.Equal(t, a.UserAgent, a.Profile.UserAgent, "%s: UA must be the profile's own", name)
	}

	// The key is the username, case-insensitively (AccountByUsername matches that way).
	upper := ParseAccounts("ALICE:p")[0]
	assert.Equal(t, find(small, "alice").Profile, upper.Profile)

	// Guard against a constant answer: distinct accounts spread over profiles.
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		seen[ParseAccounts(fmt.Sprintf("user%d:p", i))[0].UserAgent] = true
	}
	assert.GreaterOrEqual(t, len(seen), 4, "accounts must spread over several profiles")
}

// A profile the caller set is authoritative: NewClient must not replace it.
func TestNewClient_CallerProfileIsAuthoritative(t *testing.T) {
	startRecordingX(t)
	want := stealth.BuiltinProfiles[len(stealth.BuiltinProfiles)-1]
	acc := &Account{Username: "pinned", AuthToken: "tokP", CT0: "ct0P", active: true, Profile: want, UserAgent: want.UserAgent}
	c := newLoopbackClient(t, ClientConfig{Accounts: []*Account{acc}})
	_ = c
	assert.Equal(t, want, acc.Profile)
	assert.Equal(t, want.UserAgent, acc.UserAgent)
}

// I2 — every request an account sends carries the User-Agent and Client Hints
// of its own profile, through the pool read path and the account-pinned write
// path alike.
//
// Mutation: headers.go twitterHeaders — use defaultUserAgent whatever userAgent
// is passed => RED (an account whose profile is not the default sends the
// default Chrome UA and hints).
func TestRequests_CarryTheAccountsOwnUserAgentAndHints(t *testing.T) {
	record := startRecordingX(t)
	c := newLoopbackClient(t, ClientConfig{Accounts: ParseAccounts("alice:p:tokA:ct0A,bob:p:tokB:ct0B,carol:p:tokC:ct0C,dave:p:tokD:ct0D")})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{"alice", "bob", "carol", "dave"} {
		acc := c.AccountByUsername(name)
		_ = c.LikeTweet(ctx, acc, "1") // account-pinned POST; the fake's answer is irrelevant
	}
	for i := 0; i < 12 && len(distinctWireTokens(record())) < 4; i++ {
		_, _ = c.GetUserTweets(ctx, "999", 20)
	}

	reqs := record()
	require.Len(t, distinctWireTokens(reqs), 4)
	nonDefault := 0
	for _, r := range reqs {
		acc := c.AccountByUsername(map[string]string{"tokA": "alice", "tokB": "bob", "tokC": "carol", "tokD": "dave"}[r.token])
		require.NotNil(t, acc)
		ua := r.h.Get("User-Agent")
		assert.Equal(t, acc.Profile.UserAgent, ua, "%s %s from %s", r.method, r.path, acc.Username)
		hints := stealth.ClientHintsHeaders(acc.Profile.UserAgent)
		assert.Equal(t, hints["sec-ch-ua"], r.h.Get("Sec-Ch-Ua"), "client hints must match the profile UA")
		assert.Equal(t, hints["sec-ch-ua-platform"], r.h.Get("Sec-Ch-Ua-Platform"))
		if ua != defaultUserAgent {
			nonDefault++
		}
	}
	require.Positive(t, nonDefault, "no account used a non-default profile, so the test proves nothing")
}

// I2 (WithSocial shape) — the *WithSocial path rebuilds the Account and the
// Client on every call from a literal that carries no profile. Two such calls
// for the same account must present the same identity, and it must be a
// complete one (TLS profile + UA + hints), not the shared default.
func TestRebuiltAccount_PresentsSameCompleteIdentity(t *testing.T) {
	record := startRecordingX(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var uas []string
	for i := 0; i < 2; i++ {
		acc := &Account{Username: "lease-7", AuthToken: "tokL", CT0: "ct0L", active: true, credsAuthoritative: true}
		c := newLoopbackClient(t, ClientConfig{Accounts: []*Account{acc}})
		_, err := c.GetUserTweets(ctx, "999", 20)
		require.NoError(t, err)
		assert.NotEmpty(t, acc.Profile.TLSProfile, "a rebuilt account must get a full profile")
		assert.NotEmpty(t, acc.UserAgent)
		assert.Equal(t, acc.Profile.UserAgent, acc.UserAgent)
		uas = append(uas, acc.UserAgent)
	}
	assert.Equal(t, uas[0], uas[1], "same account, same identity on every call")
	for _, r := range byToken(record(), "tokL") {
		assert.Equal(t, uas[0], r.h.Get("User-Agent"))
		assert.NotEmpty(t, r.h.Get("Sec-Ch-Ua"))
	}
}

// Login-flow and guest-token requests go out on the account's own client, so
// they carry the UA that client presents (and its hints), not a fixed default.
func TestLoginFlowHeaders_UseGivenUserAgentAndHints(t *testing.T) {
	ua := stealth.BuiltinProfiles[len(stealth.BuiltinProfiles)/2].UserAgent
	h := loginFlowHeaders("gt", "", ua)
	assert.Equal(t, ua, h["user-agent"])
	if want := stealth.ClientHintsHeaders(ua); want != nil {
		assert.Equal(t, want["sec-ch-ua"], h["sec-ch-ua"])
	}
	assert.Equal(t, defaultUserAgent, loginFlowHeaders("gt", "", "")["user-agent"])
}

func distinctWireTokens(reqs []wireReq) map[string]bool {
	out := map[string]bool{}
	for _, r := range reqs {
		if r.token != "" {
			out[r.token] = true
		}
	}
	return out
}

// I3 — each account sends its own x-xp-forwarded-for: encrypted for its own
// guest id and embedding its own User-Agent; the same account yields the same
// guest id on a second Client instance.
//
// Mutation: client.go doRequestWithBody — use c.xpffGen (the shared generator)
// for every request again => RED (both accounts send one value, embedding the
// default UA, undecryptable with the per-account guest id).
func TestXpff_PerAccountIdentity(t *testing.T) {
	record := startRecordingX(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type seenXpff struct{ value, ua string }
	run := func() map[string]seenXpff {
		c := newLoopbackClient(t, ClientConfig{Accounts: ParseAccounts("alice:p:tokA:ct0A,bob:p:tokB:ct0B")})
		for _, n := range []string{"alice", "bob"} {
			_ = c.LikeTweet(ctx, c.AccountByUsername(n), "1")
		}
		out := map[string]seenXpff{}
		for _, r := range record() {
			out[r.token] = seenXpff{value: r.h.Get("X-Xp-Forwarded-For"), ua: r.h.Get("User-Agent")}
		}
		return out
	}
	first := run()
	require.NotEmpty(t, first["tokA"].value)
	require.NotEmpty(t, first["tokB"].value)
	assert.NotEqual(t, first["tokA"].value, first["tokB"].value, "accounts must not share one xpff value")

	for tok, name := range map[string]string{"tokA": "alice", "tokB": "bob"} {
		payload, err := openXpff(xpff.GuestIDFor(name), first[tok].value)
		require.NoError(t, err, "%s: xpff must be encrypted for the account's own guest id", name)
		assert.Contains(t, payload, fmt.Sprintf("%q", first[tok].ua), "%s: xpff must embed the UA the request sends", name)
	}
	_, err := openXpff(xpff.GuestIDFor("bob"), first["tokA"].value)
	assert.Error(t, err, "alice's xpff must not open with bob's guest id")

	// A second client instance for the same accounts: same guest ids.
	second := run() // record() is cumulative; the latest request per token wins
	for tok, name := range map[string]string{"tokA": "alice", "tokB": "bob"} {
		_, err := openXpff(xpff.GuestIDFor(name), second[tok].value)
		assert.NoError(t, err, "%s: guest id must be stable across Client instances", name)
	}
}

// I4 — content-type follows the request. X's web client sends it on GraphQL
// calls (its Relay fetcher appends it to every operation, GET included) and on
// bodies; a bodyless non-GraphQL GET carries none.
//
// Mutation: client.go doRequestWithBody — drop the `delete(headers,
// "content-type")` for bodyless non-GraphQL GETs => RED (the REST GET sends
// application/json).
func TestContentType_OnlyWhereTheWebClientSendsIt(t *testing.T) {
	record := startRecordingX(t)
	c := newLoopbackClient(t, ClientConfig{Accounts: ParseAccounts("alice:p:tokA:ct0A")})
	acc := c.AccountByUsername("alice")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Non-GraphQL bodyless GET (the shape of the REST reads), straight through the request layer.
	srvBase := strings.TrimSuffix(twitterBase, "/i/api/graphql")
	authTok, ct0, ua := acc.Credentials()
	_, _, _, err := c.doRequestWithBody(ctx, acc, acc.client, "GET", srvBase+"/1.1/account/settings.json", twitterHeaders(authTok, ct0, ua), nil)
	require.NoError(t, err)
	_, err = c.GetUserTweets(ctx, "999", 20) // GraphQL GET
	require.NoError(t, err)
	_ = c.LikeTweet(ctx, acc, "1") // GraphQL POST with a body

	var rest, gqlGet, gqlPost *wireReq
	for _, r := range record() {
		switch {
		case strings.HasPrefix(r.path, "/1.1/"):
			rest = &r
		case r.method == http.MethodGet:
			gqlGet = &r
		case r.method == http.MethodPost:
			gqlPost = &r
		}
	}
	require.NotNil(t, rest)
	require.NotNil(t, gqlGet)
	require.NotNil(t, gqlPost)
	assert.Empty(t, rest.h.Get("Content-Type"), "bodyless REST GET must carry no content-type")
	assert.Equal(t, "application/json", gqlGet.h.Get("Content-Type"), "GraphQL GET: the web client sends it")
	assert.Equal(t, "application/json", gqlPost.h.Get("Content-Type"))
}

// A bare-default client (no account) presents the identity of the stealth
// client it sends through: its guest-token activation and guest reads carry
// that client's own UA and Client Hints, so they stay coherent with its
// ClientHello under any go-stealth default, not only the one the constant was
// written for.
//
// Mutation: request.go doPoolRequest guest arm — pass "" instead of
// clientUA(c.client) to guestHeaders (and auth.go getGuestToken — use
// defaultUserAgent instead of clientUA(client)) => RED (131 UA over a 146 TLS).
func TestBareDefaultClient_GuestRequestsCarryTheClientsOwnIdentity(t *testing.T) {
	type seen struct{ path, ua, hints string }
	var (
		mu   sync.Mutex
		reqs []seen
	)
	g := newGuestFake(t)
	g.guestReply = func(w http.ResponseWriter, _ string, _ int32) {
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
	}
	inner := g.srv.Config.Handler
	g.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, seen{r.URL.Path, r.Header.Get("User-Agent"), r.Header.Get("Sec-Ch-Ua")})
		mu.Unlock()
		inner.ServeHTTP(w, r)
	})

	// A profile that is not go-stealth's default, so the constant cannot match by luck.
	c := newLoopbackClient(t, ClientConfig{stealthOpts: []stealth.ClientOption{stealth.WithProfile(stealth.ProfileChrome146)}})
	c.cfg.DisableGuestFallback = false
	id := c.client.Identity()
	require.Equal(t, stealth.ProfileChrome146, id.TLSProfile)
	require.Contains(t, id.UserAgent, "Chrome/146.", "go-stealth must pair the 146 ClientHello with a 146 UA")
	require.NotEqual(t, defaultUserAgent, id.UserAgent)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.acquireGuestToken(ctx, c.client)
	require.NoError(t, err)
	c.setGuestToken("cached-guest")
	_, _, _ = c.doGET(ctx, "UserByRestId", g.srv.URL+"/i/api/graphql/x/UserByRestId")

	mu.Lock()
	defer mu.Unlock()
	var activate, guestRead bool
	for _, r := range reqs {
		if strings.HasSuffix(r.path, "/guest/activate.json") {
			activate = true
		} else if strings.Contains(r.path, "UserByRestId") {
			guestRead = true
		} else {
			continue
		}
		assert.Equal(t, id.UserAgent, r.ua, "%s must carry the client's own UA", r.path)
		assert.Equal(t, id.ClientHints["sec-ch-ua"], r.hints, "%s must carry the client's own hints", r.path)
	}
	assert.True(t, activate, "no guest activation seen")
	assert.True(t, guestRead, "no guest read seen")
}

// An account written as a struct literal with a proxy and no profile (what the
// *WithSocial lease builds) gets a client: the identity is completed before the
// client is built, so an empty profile is never handed to go-stealth.
func TestNewClient_LiteralAccountWithProxyAndNoProfileBuildsAClient(t *testing.T) {
	acc := &Account{Username: "leased", AuthToken: "tokX", CT0: "ct0X", Proxy: deadProxy, active: true, credsAuthoritative: true}
	c := newLoopbackClient(t, ClientConfig{Accounts: []*Account{acc}})
	require.NotNil(t, acc.client, "the account's client must have been built")
	assert.False(t, acc.proxyClientFailed)
	assert.NotEmpty(t, acc.Profile.TLSProfile)
	assert.Equal(t, acc.UserAgent, acc.client.Identity().UserAgent)
	assert.Equal(t, acc.Profile.TLSProfile, acc.client.Identity().TLSProfile)
	_ = c
}

// assignmentFor returns the (browser, OS, TLS) triple each synthetic username
// is assigned.
func assignmentFor(n int) []string {
	out := make([]string, n)
	for i := range out {
		p := profileForAccountKey(accountKey(fmt.Sprintf("synthetic-user-%d", i)))
		out[i] = p.Browser + "/" + p.OS + "/" + string(p.TLSProfile)
	}
	return out
}

func withBuiltinProfiles(t *testing.T, profiles []stealth.BrowserProfile) {
	t.Helper()
	orig := stealth.BuiltinProfiles
	stealth.BuiltinProfiles = profiles
	t.Cleanup(func() { stealth.BuiltinProfiles = orig })
}

// A profile's identity is browser + OS + TLS fingerprint, not its UA text:
// editing UA strings in go-stealth moves no account, and adding one profile
// moves only the accounts it wins (its weight over the new total).
//
// Mutation: identity.go profileForAccountKey — put p.UserAgent back into the
// hashed key => RED (editing UA text reshuffles the assignment).
func TestProfileAssignment_KeysOnProfileIdentityNotUserAgentText(t *testing.T) {
	const users = 2000
	base := assignmentFor(users)

	edited := make([]stealth.BrowserProfile, len(stealth.BuiltinProfiles))
	copy(edited, stealth.BuiltinProfiles)
	for i := range edited {
		edited[i].UserAgent += " (edited)"
	}
	orig := stealth.BuiltinProfiles
	withBuiltinProfiles(t, edited)
	assert.Equal(t, base, assignmentFor(users), "editing UA text must move no account")

	var total float64
	for _, p := range identityCandidates() {
		total += identityShareWeights[osBrowserKey(p)]
	}
	added := append(append([]stealth.BrowserProfile(nil), orig...), stealth.BrowserProfile{
		UserAgent: "x", TLSProfile: "chrome_999", Browser: "chrome", OS: "windows",
	})
	withBuiltinProfiles(t, added)
	moved := 0
	for i, a := range assignmentFor(users) {
		if a != base[i] {
			moved++
			assert.Equal(t, "chrome/windows/chrome_999", a, "an account may only move to the newcomer")
		}
	}
	newcomer := identityShareWeights["chrome/windows"]
	want := float64(users) * newcomer / (total + newcomer)
	assert.InDelta(t, want, float64(moved), want*0.25, "adding a profile should move about its weight's share of accounts")
}

// Login-flow requests (guest activation, flow init, every step) go out on the
// account's own client and must carry that account's UA and Client Hints.
//
// Mutation: auth.go initLoginFlowFull / submitFlowStep — pass defaultUserAgent
// instead of clientUA(client) to loginFlowHeaders => RED.
func TestLoginFlow_CarriesTheAccountsOwnIdentity(t *testing.T) {
	type seen struct{ path, query, ua, hints string }
	var (
		mu   sync.Mutex
		reqs []seen
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, seen{r.URL.Path, r.URL.RawQuery, r.Header.Get("User-Agent"), r.Header.Get("Sec-Ch-Ua")})
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/guest/activate.json"):
			_, _ = w.Write([]byte(`{"guest_token":"g1"}`))
		case strings.Contains(r.URL.RawQuery, "flow_name=login"):
			_, _ = w.Write([]byte(`{"flow_token":"f1","subtasks":[{"subtask_id":"LoginEnterPassword"}]}`))
		default:
			_, _ = w.Write([]byte(`{"flow_token":"f2","subtasks":[{"subtask_id":"LoginSuccessSubtask"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	orig := twitterAPIURL
	twitterAPIURL = srv.URL
	t.Cleanup(func() { twitterAPIURL = orig })

	var prof stealth.BrowserProfile
	for _, p := range stealth.BuiltinProfiles {
		if p.TLSProfile == stealth.ProfileChrome146 && p.OS == "linux" {
			prof = p
		}
	}
	require.NotEmpty(t, prof.UserAgent)
	require.NotEqual(t, defaultUserAgent, prof.UserAgent)
	acc := &Account{Username: "loginer", Password: "pw", AuthToken: "tokQ", CT0: "ct0Q", active: true, Profile: prof, UserAgent: prof.UserAgent}
	c := newLoopbackClient(t, ClientConfig{Accounts: []*Account{acc}})

	_ = c.login(acc, acc.client) // ends without an auth_token cookie on the loopback; only the requests matter

	hints := stealth.ClientHintsHeaders(prof.UserAgent)["sec-ch-ua"]
	require.NotEmpty(t, hints)
	mu.Lock()
	defer mu.Unlock()
	var activate, initFlow, step bool
	for _, r := range reqs {
		switch {
		case strings.HasSuffix(r.path, "/guest/activate.json"):
			activate = true
		case strings.Contains(r.query, "flow_name=login"):
			initFlow = true
		case strings.HasSuffix(r.path, "/onboarding/task.json"):
			step = true
		default:
			continue
		}
		assert.Equal(t, prof.UserAgent, r.ua, "%s?%s", r.path, r.query)
		assert.Equal(t, hints, r.hints, "%s?%s", r.path, r.query)
	}
	assert.True(t, activate && initFlow && step, "login flow did not reach all three request kinds: %v %v %v", activate, initFlow, step)
}

// A caller-set UA that no built-in profile presents stays the account's
// identity everywhere, login flow included: the client reports it over
// go-stealth's default TLS profile.
func TestNewClient_UnmatchedCallerUserAgentIsTheClientsIdentity(t *testing.T) {
	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/99.0.0.0 Safari/537.36"
	acc := &Account{Username: "custom", AuthToken: "tokU", CT0: "ct0U", active: true, UserAgent: ua}
	_ = newLoopbackClient(t, ClientConfig{Accounts: []*Account{acc}})
	require.NotNil(t, acc.client)
	assert.Equal(t, ua, acc.client.Identity().UserAgent)
	assert.Equal(t, ua, acc.UserAgent)
	assert.Equal(t, defaultTLSProfile, acc.client.Identity().TLSProfile)
}
