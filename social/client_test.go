package social

import (
	"context"
	"encoding/json"
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
