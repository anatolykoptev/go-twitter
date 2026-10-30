package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/social"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const socialCalls = 20

const socialUserBody = `{"data":{"user":{"result":{"__typename":"User","id":"VXNlcjo5OTk=","rest_id":"999",` +
	`"core":{"name":"Tesla","screen_name":"tesla","created_at":"Mon Jan 02 15:04:05 +0000 2021"}}}}}`

// countingMP wraps a MeterProvider and counts callback registrations and
// unregistrations made through any meter it hands out.
type countingMP struct {
	metric.MeterProvider
	regs, unregs atomic.Int32
}

func (p *countingMP) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	return &countingMeter{Meter: p.MeterProvider.Meter(name, opts...), p: p}
}

type countingMeter struct {
	metric.Meter
	p *countingMP
}

func (m *countingMeter) RegisterCallback(f metric.Callback, i ...metric.Observable) (metric.Registration, error) {
	reg, err := m.Meter.RegisterCallback(f, i...)
	if err != nil {
		return reg, err
	}
	m.p.regs.Add(1)
	return &countingReg{Registration: reg, p: m.p}, nil
}

type countingReg struct {
	metric.Registration
	p *countingMP
}

func (r *countingReg) Unregister() error {
	r.p.unregs.Add(1)
	return r.Registration.Unregister()
}

// socialRig is the *WithSocial journey against fakes: a go-social credential
// server alternating between two accounts, a fake x.com API, and a fake warm
// backend standing in for the x.com pages the xtid manager scrapes.
type socialRig struct {
	sc      *social.Client
	warm    *fakeWarmBackend
	reader  *sdkmetric.ManualReader
	mp      *sdkmetric.MeterProvider
	acquire atomic.Int32

	// reports records every usage status POSTed to the fake go-social
	// (ReportUsage), in order — the observable surface for "a request outcome
	// must not mark a healthy leased account failed in the shared pool".
	reportsMu sync.Mutex
	reports   []string
	// reportBodies / acquireQueries record the full decoded report bodies and
	// the raw acquire query strings, for the rate-limit carry-over journey.
	reportBodies   []map[string]any
	acquireQueries []string

	// leaseProxy, when set, is stamped onto every Credentials the fake
	// go-social hands out — the seam for a malformed/dead-proxy lease.
	leaseProxy string

	// sameAccount makes the fake go-social lease one account every time, and
	// keepPacer keeps the production account pacer on the per-call clients
	// (the rig otherwise disables it to stay fast).
	sameAccount, keepPacer bool
}

func newSocialRig(t *testing.T) *socialRig {
	return newSocialRigX(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "UserByScreenName") {
			_, _ = w.Write([]byte(socialUserBody))
			return
		}
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now().Add(-5 * time.Second)))))
	})
}

// newSocialRigX is newSocialRig with an injectable fake-x.com handler, for
// journeys where X answers with a fault instead of data.
func newSocialRigX(t *testing.T, xHandler http.HandlerFunc) *socialRig {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // sessions are persisted under $HOME

	r := &socialRig{warm: &fakeWarmBackend{}}
	bc := startFakeX(t, xHandler)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			var rep struct {
				Status string `json:"status"`
			}
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &rep)
			var full map[string]any
			_ = json.Unmarshal(raw, &full)
			r.reportsMu.Lock()
			r.reports = append(r.reports, rep.Status)
			r.reportBodies = append(r.reportBodies, full)
			r.reportsMu.Unlock()
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if req.Method != http.MethodGet {
			return
		}
		r.reportsMu.Lock()
		r.acquireQueries = append(r.acquireQueries, req.URL.RawQuery)
		r.reportsMu.Unlock()
		who := "a"
		if r.acquire.Add(1)%2 == 0 && !r.sameAccount {
			who = "b"
		}
		_ = json.NewEncoder(w).Encode(social.Credentials{
			ID:    "acc-" + who,
			Proxy: r.leaseProxy,
			Credentials: map[string]string{
				"username": "user_" + who, "auth_token": "tok" + who, "ct0": "ct0" + who,
			},
		})
	}))
	t.Cleanup(srv.Close)
	r.sc = social.NewClient(srv.URL, "tok", "test")

	r.reader = sdkmetric.NewManualReader()
	r.mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(r.reader))
	t.Cleanup(func() { _ = r.mp.Shutdown(context.Background()) })

	// Fresh production-built shared manager per test; restored afterwards.
	socialXtidMu.Lock()
	prev := socialXtidMgr
	socialXtidMgr = nil
	socialXtidMu.Unlock()
	testHookSocialClient = func(c *Client) {
		c.client = bc
		c.warmDoerOverride = r.warm
		if !r.keepPacer {
			c.accountPacer = nil
		}
	}
	t.Cleanup(func() {
		testHookSocialClient = nil
		SetSocialMeterProvider(nil)
		socialXtidMu.Lock()
		socialXtidMgr = prev
		socialXtidMu.Unlock()
	})
	return r
}

func (r *socialRig) collect(t *testing.T) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, r.reader.Collect(context.Background(), &rm))
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// F1 — journey: 20 GetUserTweetsWithSocial calls fetch the x.com warm page once.
func TestSocial_SharedXtid_WarmPageFetchedOnceAcross20Calls(t *testing.T) {
	r := newSocialRig(t)
	for i := 0; i < socialCalls; i++ {
		tweets, err := GetUserTweetsWithSocial(context.Background(), r.sc, "tesla", 5)
		require.NoError(t, err, "call %d", i)
		require.NotEmpty(t, tweets)
	}
	r.warm.mu.Lock()
	defer r.warm.mu.Unlock()
	assert.Equal(t, 1, r.warm.hits["https://x.com/tesla"],
		"warm page must be fetched once across %d *WithSocial calls", socialCalls)
}

// F2a — per-call clients never register the pool.accounts gauge.
func TestSocial_PerCallClientsRegisterNoPoolGauge(t *testing.T) {
	r := newSocialRig(t)
	cmp := &countingMP{MeterProvider: r.mp}
	SetSocialMeterProvider(cmp)
	for i := 0; i < socialCalls; i++ {
		_, err := GetUserTweetsWithSocial(context.Background(), r.sc, "tesla", 5)
		require.NoError(t, err)
	}
	assert.Zero(t, cmp.regs.Load(), "per-call clients must not register callbacks")
	_, present := r.collect(t)[metricPoolAccounts]
	assert.False(t, present, "pool.accounts must not be reported by per-call clients")
}

// F2b — whatever a per-call client registered is unregistered when the call
// ends, on the success path and the error path.
func TestSocial_TryWithAccountClosesClientOnEveryPath(t *testing.T) {
	r := newSocialRig(t)
	cmp := &countingMP{MeterProvider: r.mp}
	prevHook := testHookSocialClient
	testHookSocialClient = func(c *Client) {
		prevHook(c)
		c.registerPoolGauge(cmp) // a client that does own a gauge callback
	}
	for i := 0; i < socialCalls; i++ {
		_, err := GetUserTweetsWithSocial(context.Background(), r.sc, "tesla", 5)
		require.NoError(t, err)
	}
	_, err := tryWithAccount(context.Background(), r.sc, "", func(context.Context, *Client) (int, error) {
		return 0, errors.New("boom")
	})
	require.Error(t, err)
	assert.EqualValues(t, socialCalls+1, cmp.regs.Load())
	assert.Equal(t, cmp.regs.Load(), cmp.unregs.Load(), "every registered callback must be unregistered")
}

// F3 — a MeterProvider set via SetSocialMeterProvider receives the metrics.
func TestSocial_MeterProviderRecordsDurationAndFreshness(t *testing.T) {
	r := newSocialRig(t)
	SetSocialMeterProvider(r.mp)
	_, err := GetUserTweetsWithSocial(context.Background(), r.sc, "tesla", 5)
	require.NoError(t, err)

	got := r.collect(t)
	ops := map[string]bool{}
	for _, p := range histPoints(t, got, metricRequestDuration) {
		ops[attrMap(p.Attributes)[attrOperation]] = true
	}
	assert.True(t, ops["UserTweets"], "request.duration must carry operation=UserTweets, got %v", ops)
	fresh := histPoints(t, got, metricFreshness)
	require.Len(t, fresh, 1)
	assert.Equal(t, "UserTweets", attrMap(fresh[0].Attributes)[attrOperation])
}

// F4 — each call's warm fetch carries THAT call's account cookie. The shared
// manager re-initialises on every request here (1ns refresh) so a second warm
// fetch actually happens in call 2.
func TestSocial_SharedXtid_WarmFetchUsesCurrentCallCookie(t *testing.T) {
	r := newSocialRig(t)
	mgr := xtid.NewManager(failFetcher{})
	mgr.SetRefreshInterval(time.Nanosecond)
	socialXtidMu.Lock()
	socialXtidMgr = mgr
	socialXtidMu.Unlock()

	for i := 0; i < 2; i++ {
		_, err := GetUserWithSocial(context.Background(), r.sc, "tesla")
		require.NoError(t, err)
	}
	r.warm.mu.Lock()
	defer r.warm.mu.Unlock()
	require.Len(t, r.warm.cookies, 2)
	assert.Contains(t, r.warm.cookies[0], "auth_token=toka")
	assert.Contains(t, r.warm.cookies[1], "auth_token=tokb",
		"call 2 warm fetch must use call 2's cookie, not call 1's")
}

// The per-call clients of the *WithSocial path must share one pacer keyed by
// account: two consecutive calls that lease the same account are spaced by the
// pace even though each builds its own Client. Options passed to the helper
// reach the per-call client (the 1500ms pace here).
//
// Mutation: social.go tryWithAccount, `cfg.sharedPacer = socialPacerFor(cfg)` ->
// `cfg.sharedPacer = nil` (a fresh per-client pacer again) => RED:
// "two *WithSocial calls on one account were not spaced by the account pace".
func TestSocial_AccountPacerSharedAcrossPerCallClients(t *testing.T) {
	const pace = 1500 * time.Millisecond
	r := newSocialRig(t)
	r.sameAccount, r.keepPacer = true, true
	t.Cleanup(func() {
		socialPacerMu.Lock()
		socialPacers = map[[2]time.Duration]*ratelimit.KeyedPacer{}
		socialPacerMu.Unlock()
	})
	ctx := context.Background()

	// Warm-up with pacing off: one-time xtid/session setup is not part of the span.
	_, err := GetUserWithSocial(ctx, r.sc, "tesla", WithAccountPace(0, 0))
	require.NoError(t, err)

	start := time.Now()
	for i := 0; i < 2; i++ {
		_, err = GetUserWithSocial(ctx, r.sc, "tesla", WithAccountPace(pace, 0))
		require.NoError(t, err)
	}
	assert.GreaterOrEqual(t, time.Since(start), pace-50*time.Millisecond,
		"two *WithSocial calls on one account were not spaced by the account pace")
}

// Every *WithSocial helper must hand its trailing options to the per-call client.
//
// Mutation: social.go GetFollowingWithSocial, drop `, opts...` from the
// withSocialAccount call => TestSocial_AllHelpersApplyOptions/GetFollowing goes
// RED (`option was not applied`).
func TestSocial_AllHelpersApplyOptions(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(sc *social.Client, opt ClientOption){
		"Search": func(sc *social.Client, o ClientOption) { _, _ = SearchWithSocial(ctx, sc, "q", 5, o) },
		"SearchTweets": func(sc *social.Client, o ClientOption) {
			_, _ = SearchTweetsWithSocial(ctx, sc, "q", SearchLatest, 5, o)
		},
		"SearchUsers":          func(sc *social.Client, o ClientOption) { _, _ = SearchUsersWithSocial(ctx, sc, "q", 5, o) },
		"GetUser":              func(sc *social.Client, o ClientOption) { _, _ = GetUserWithSocial(ctx, sc, "tesla", o) },
		"GetUserTweets":        func(sc *social.Client, o ClientOption) { _, _ = GetUserTweetsWithSocial(ctx, sc, "tesla", 5, o) },
		"GetTweetConversation": func(sc *social.Client, o ClientOption) { _, _, _ = GetTweetConversationWithSocial(ctx, sc, "1", o) },
		"GetUserTweetsByID":    func(sc *social.Client, o ClientOption) { _, _ = GetUserTweetsByIDWithSocial(ctx, sc, "999", 5, o) },
		"GetFollowing":         func(sc *social.Client, o ClientOption) { _, _ = GetFollowingWithSocial(ctx, sc, "999", 5, o) },
		"GetFollowers":         func(sc *social.Client, o ClientOption) { _, _ = GetFollowersWithSocial(ctx, sc, "999", 5, o) },
		"GetRetweeters":        func(sc *social.Client, o ClientOption) { _, _ = GetRetweetersWithSocial(ctx, sc, "1", 5, o) },
		"GetTweetByID":         func(sc *social.Client, o ClientOption) { _, _ = GetTweetByIDWithSocial(ctx, sc, "1", o) },
	}
	require.Len(t, calls, 11, "one entry per *WithSocial helper")
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			r := newSocialRig(t)
			var applied atomic.Int32
			opt := func(cfg *ClientConfig) {
				applied.Add(1)
				WithAccountPace(0, 0)(cfg) // keep the journey fast
			}
			done := make(chan struct{})
			go func() { defer close(done); call(r.sc, opt) }()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("helper did not return")
			}
			assert.Positive(t, applied.Load(), "option was not applied")
		})
	}
}
