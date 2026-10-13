package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
}

func newSocialRig(t *testing.T) *socialRig {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // sessions are persisted under $HOME

	r := &socialRig{warm: &fakeWarmBackend{}}
	bc := startFakeX(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "UserByScreenName") {
			_, _ = w.Write([]byte(socialUserBody))
			return
		}
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now().Add(-5 * time.Second)))))
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			return
		}
		who := "a"
		if r.acquire.Add(1)%2 == 0 {
			who = "b"
		}
		_ = json.NewEncoder(w).Encode(social.Credentials{
			ID: "acc-" + who,
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
		c.accountPacer = nil
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
	_, err := tryWithAccount(context.Background(), r.sc, func(context.Context, *Client) (int, error) {
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
