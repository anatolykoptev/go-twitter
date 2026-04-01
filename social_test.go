package twitter

import (
	"context"
	"encoding/json"
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
