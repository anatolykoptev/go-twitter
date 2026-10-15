package social

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/twitter/account", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "test-consumer", r.Header.Get("X-Consumer"))

		_ = json.NewEncoder(w).Encode(Credentials{
			ID:          "acc-123",
			Credentials: map[string]string{"username": "u", "auth_token": "t", "ct0": "c"},
			Proxy:       "http://proxy:8080",
			ExpiresIn:   300,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token", "test-consumer")
	creds, err := c.AcquireAccount(context.Background(), "twitter")
	require.NoError(t, err)
	assert.Equal(t, "acc-123", creds.ID)
	assert.Equal(t, "u", creds.Credentials["username"])
	assert.Equal(t, "http://proxy:8080", creds.Proxy)
}

func TestAcquireAccount_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "consumer")
	_, err := c.AcquireAccount(context.Background(), "twitter")
	assert.ErrorContains(t, err, "503")
}

func TestReportUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/twitter/report/acc-123", r.URL.Path)
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "consumer")
	err := c.ReportUsage(context.Background(), "twitter", "acc-123", "success")
	assert.NoError(t, err)
}

func TestAcquireAccountFor_SendsEndpointHint(t *testing.T) {
	cases := []struct {
		name, endpoint, wantQuery string
	}{
		{"hint", "SearchTimeline", "endpoint=SearchTimeline"},
		{"escaped", "a&b=c", "endpoint=a%26b%3Dc"},
		{"empty sends no parameter", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				assert.Equal(t, "/twitter/account", r.URL.Path)
				_ = json.NewEncoder(w).Encode(Credentials{ID: "acc-1"})
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "tok", "consumer")
			creds, err := c.AcquireAccountFor(context.Background(), "twitter", tc.endpoint)
			require.NoError(t, err)
			assert.Equal(t, "acc-1", creds.ID)
			assert.Equal(t, tc.wantQuery, gotQuery)
		})
	}
}

func TestReportUsageWithRateLimit_Body(t *testing.T) {
	cases := []struct {
		name string
		rl   *RateLimit
		want string
	}{
		{"nil is the legacy body", nil, `{"status":"success"}`},
		{
			"full window",
			&RateLimit{Endpoint: "SearchTimeline", Limit: 50, Remaining: 0, Reset: 1760000000},
			`{"status":"success","rate_limit":{"endpoint":"SearchTimeline","limit":50,"remaining":0,"reset":1760000000}}`,
		},
		{
			"unknown limit is omitted, zero remaining is kept",
			&RateLimit{Endpoint: "UserTweets", Remaining: 0, Reset: 1760000001},
			`{"status":"success","rate_limit":{"endpoint":"UserTweets","remaining":0,"reset":1760000001}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got = string(b)
				assert.Equal(t, "/twitter/report/acc-1", r.URL.Path)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "tok", "consumer")
			require.NoError(t, c.ReportUsageWithRateLimit(context.Background(), "twitter", "acc-1", "success", tc.rl))
			assert.JSONEq(t, tc.want, got)
		})
	}
}
