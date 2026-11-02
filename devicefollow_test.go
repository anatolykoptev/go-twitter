package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The request must carry every Relay provided variable the operation declares;
// the server answers 422 when one is missing. It must also carry the count and
// no `features` parameter (the x-web persisted query declares none).
//
// Mutation: devicefollow.go tweetNotificationsVariables, delete the loop that
// copies tweetNotificationsProviders into v => this test goes RED
// ("provided variable missing").
func TestTweetNotificationsURL_CarriesProviders(t *testing.T) {
	raw, err := tweetNotificationsURL(20)
	require.NoError(t, err)
	u, err := url.Parse(raw)
	require.NoError(t, err)

	assert.Contains(t, u.Path, "/tweetNotificationsQuery")
	assert.Empty(t, u.Query().Get("features"))
	var vars map[string]any
	require.NoError(t, json.Unmarshal([]byte(u.Query().Get("variables")), &vars))
	assert.EqualValues(t, 20, vars["count"])

	want := []string{
		"__relay_internal__pv__xclientsfeaturesappviewerisloggedinrelayprovider",
		"__relay_internal__pv__xclientsfeaturestranslationgroktranslatedpostenabledrelayprovider",
		"__relay_internal__pv__xclientsfeaturestweetcomponentsgrokanalyzepostrelayprovider",
		"__relay_internal__pv__xclientsfeaturestranslationnoteautotranslationenabledrelayprovider",
		"__relay_internal__pv__liveeventpostcontextenabledrelayprovider",
		"__relay_internal__pv__sportspostcontextenabledrelayprovider",
	}
	for _, k := range want {
		v, ok := vars[k]
		require.True(t, ok, "provided variable missing: %s", k)
		assert.IsType(t, true, v, "provided variable %s must be a boolean", k)
	}
	assert.Len(t, vars, len(want)+1)
}

func TestDeviceNotifications(t *testing.T) {
	tw := []*Tweet{{ID: "1"}}
	gq := []*Tweet{{ID: "2"}}
	boom := errors.New("boom")
	gone := func(status int) error { return newAPIError(status, []byte(`{"errors":[{"message":"gone"}]}`), nil) }

	src := func(ts []*Tweet, err error, calls *int) feedFetcher {
		return func(context.Context, int) ([]*Tweet, error) { *calls++; return ts, err }
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name        string
		ctx         context.Context
		rest, gql   func(*int) feedFetcher
		want        []*Tweet
		wantErr     bool
		restN, gqlN int
	}{
		{"rest has tweets", context.Background(), func(n *int) feedFetcher { return src(tw, nil, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, tw, false, 1, 0},
		{"rest empty is a quiet poll: no fallback", context.Background(), func(n *int) feedFetcher { return src(nil, nil, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, nil, false, 1, 0},
		{"rest 404, graphql ok", context.Background(), func(n *int) feedFetcher { return src(nil, gone(404), n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, gq, false, 1, 1},
		{"rest 410, graphql ok", context.Background(), func(n *int) feedFetcher { return src(nil, gone(410), n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, gq, false, 1, 1},
		{"rest 503 endpoint fault, graphql ok", context.Background(), func(n *int) feedFetcher { return src(nil, gone(503), n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, gq, false, 1, 1},
		{"rest 404, graphql fails: error", context.Background(), func(n *int) feedFetcher { return src(nil, gone(404), n) }, func(n *int) feedFetcher { return src(nil, boom, n) }, nil, true, 1, 1},
		{"rest other error: no fallback", context.Background(), func(n *int) feedFetcher { return src(nil, boom, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, nil, true, 1, 0},
		{"cancelled ctx: error, no fallback", cancelled, func(n *int) feedFetcher { return src(nil, gone(404), n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, nil, true, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var restN, gqlN int
			got, err := deviceNotifications(tc.ctx, 20, tc.rest(&restN), tc.gql(&gqlN), nil)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.restN, restN, "rest calls")
			assert.Equal(t, tc.gqlN, gqlN, "graphql calls")
		})
	}

	t.Run("cancelled ctx surfaces context.Canceled", func(t *testing.T) {
		var n int
		_, err := deviceNotifications(cancelled, 20, func(ctx context.Context, _ int) ([]*Tweet, error) { return nil, ctx.Err() }, src(gq, nil, &n), nil)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, n)
	})
	t.Run("both sources broken keeps both errors", func(t *testing.T) {
		var n int
		_, err := deviceNotifications(context.Background(), 20, src(nil, gone(404), &n), src(nil, boom, &n), nil)
		require.ErrorIs(t, err, ErrEndpoint)
		require.ErrorIs(t, err, boom)
	})
}

// GetDeviceNotifications end to end against a fake x.com: it must serve the
// REST feed when REST answers (GraphQL untouched) and fall back to GraphQL only
// when REST is gone.
//
// Mutation: devicefollow.go GetDeviceNotifications, swap the two arguments of
// the deviceNotifications call (c.GetTweetNotifications, c.GetDeviceFollowTweets)
// => TestGetDeviceNotifications_EndToEnd/rest_serves_and_graphql_untouched goes
// RED (the GraphQL tweets come back and the GraphQL endpoint is hit).
func TestGetDeviceNotifications_EndToEnd(t *testing.T) {
	restBody, err := os.ReadFile("testdata/device_follow.json")
	require.NoError(t, err)
	gqlBody, err := os.ReadFile("testdata/tweet_notifications.json")
	require.NoError(t, err)

	for _, tc := range []struct {
		name       string
		restStatus int
		wantFirst  string
		wantRest   int32
		wantGQL    int32
	}{
		{"rest serves and graphql untouched", http.StatusOK, "1985000000000000003", 1, 0},
		{"rest gone falls back to graphql", http.StatusNotFound, "1985000000000000001", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var restHits, gqlHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "device_follow"):
					restHits.Add(1)
					w.WriteHeader(tc.restStatus)
					if tc.restStatus == http.StatusOK {
						_, _ = w.Write(restBody)
					} else {
						_, _ = w.Write([]byte(`{"errors":[{"message":"Gone"}]}`))
					}
				case strings.Contains(r.URL.Path, "tweetNotificationsQuery"):
					gqlHits.Add(1)
					_, _ = w.Write(gqlBody)
				default:
					w.WriteHeader(http.StatusTeapot)
				}
			}))
			t.Cleanup(srv.Close)
			origBase, origREST := twitterBase, deviceFollowURL
			twitterBase = srv.URL + "/i/api/graphql"
			deviceFollowURL = srv.URL + "/i/api/2/notifications/device_follow.json"
			t.Cleanup(func() { twitterBase, deviceFollowURL = origBase, origREST })

			c, _ := newPoolWaitClient(t, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			tweets, err := c.GetDeviceNotifications(ctx, 20)
			require.NoError(t, err)
			require.NotEmpty(t, tweets)
			assert.Equal(t, tc.wantFirst, tweets[0].ID)
			assert.Equal(t, tc.wantRest, restHits.Load(), "REST hits")
			assert.Equal(t, tc.wantGQL, gqlHits.Load(), "GraphQL hits")
		})
	}
}

// Both device-notification sources are per-account feeds: no guest fallback.
//
// Mutation: request.go requiresAuth, remove "DeviceFollow", "tweetNotificationsQuery"
// from the case list => TestRequiresAuth_DeviceNotificationOps goes RED
// (`op "DeviceFollow" must require auth`).
func TestRequiresAuth_DeviceNotificationOps(t *testing.T) {
	for _, op := range []string{deviceFollowEndpoint, tweetNotificationsOp} {
		assert.True(t, requiresAuth(op), "op %q must require auth (no guest fallback)", op)
	}
}

func TestWarnLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	w := &warnLimiter{every: time.Minute, now: func() time.Time { return now }}
	ok, n := w.allow()
	assert.True(t, ok)
	assert.Zero(t, n)
	for i := 0; i < 3; i++ {
		ok, _ = w.allow()
		assert.False(t, ok)
	}
	now = now.Add(61 * time.Second)
	ok, n = w.allow()
	assert.True(t, ok)
	assert.Equal(t, 3, n, "suppressed repeats are reported with the next emission")
}
