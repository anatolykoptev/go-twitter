package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anatolykoptev/go-twitter/social"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchWithSocial_AcquireError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	sc := social.NewClient(srv.URL, "tok", "test")
	_, err := SearchWithSocial(context.Background(), sc, "golang", 10)
	require.Error(t, err)
	assert.ErrorContains(t, err, "all 6 accounts failed")
	assert.ErrorContains(t, err, "acquire account")
}

func TestSearchWithSocial_ReportsErrorOnFailure(t *testing.T) {
	var reportedStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(social.Credentials{
				ID: "acc-1",
				Credentials: map[string]string{
					"username":   "test_social_user_nonexistent",
					"auth_token": "fake_at",
					"ct0":        "fake_ct0",
				},
				Proxy: "http://127.0.0.1:1", // unreachable proxy
			})
		case r.Method == http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			reportedStatus = body["status"]
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sc := social.NewClient(srv.URL, "tok", "test")
	_, err := SearchWithSocial(ctx, sc, "test", 5)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "all 6 accounts failed")
	assert.Equal(t, "auth_error", reportedStatus)
}

func TestSearchUsersWithSocial_AcquireError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	sc := social.NewClient(srv.URL, "tok", "test")
	_, err := SearchUsersWithSocial(context.Background(), sc, "golang", 10)
	require.Error(t, err)
	assert.ErrorContains(t, err, "all 6 accounts failed")
	assert.ErrorContains(t, err, "acquire account")
}

func TestGetTweetConversationWithSocial_AcquireError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	sc := social.NewClient(srv.URL, "tok", "test")
	_, _, err := GetTweetConversationWithSocial(context.Background(), sc, "123")
	require.Error(t, err)
	assert.ErrorContains(t, err, "all 6 accounts failed")
	assert.ErrorContains(t, err, "acquire account")
}

// TestSearchTweets_InvalidProduct: validation must reject an unknown tab
// BEFORE any account is acquired — a typo'd product must not burn pool calls.
func TestSearchTweets_InvalidProduct(t *testing.T) {
	var acquires int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		acquires++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tw, err := NewClient(ClientConfig{})
	require.NoError(t, err)
	_, err = tw.SearchTweets(context.Background(), "q", SearchProduct("bogus"), 5)
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid product")

	sc := social.NewClient(srv.URL, "tok", "test")
	_, err = SearchTweetsWithSocial(context.Background(), sc, "q", SearchProduct("bogus"), 5)
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid product")
	assert.Zero(t, acquires, "invalid product must not acquire an account")
}

// TestWithSocial_NotFoundNoRetry: a deterministic ErrNotFound (deleted tweet,
// suspended handle) must report the account as SUCCESS — it did its job — and
// must NOT burn the remaining pool retries.
func TestWithSocial_NotFoundNoRetry(t *testing.T) {
	var acquires int
	var lastReport string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			acquires++
			_ = json.NewEncoder(w).Encode(social.Credentials{
				ID:          "acc-1",
				Credentials: map[string]string{"username": "u", "auth_token": "t", "ct0": "c"},
				Proxy:       "http://127.0.0.1:1",
			})
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		lastReport = body["status"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sc := social.NewClient(srv.URL, "tok", "test")
	_, err := withSocialAccount(context.Background(), sc, func(context.Context, *Client) (*Tweet, error) {
		return nil, fmt.Errorf("tweet 123 absent: %w", ErrNotFound)
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, 1, acquires, "not-found must not retry across accounts")
	assert.Equal(t, "success", lastReport, "not-found is a healthy-account outcome")
}
