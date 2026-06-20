package bundle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCheckBundleRedirect_BlocksDisallowed proves the HTTP client's
// CheckRedirect refuses to follow a 302 that points at a disallowed host (here
// the metadata-style link-local address) or downgrades to http. A compromised
// x.com/CDN response returning `302 Location: http://169.254.169.254/…` must
// not be followed.
func TestCheckBundleRedirect_BlocksDisallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	// Same client wiring NewHTTPFetcher uses.
	client := &http.Client{CheckRedirect: checkBundleRedirect}
	resp, err := client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the redirect to a disallowed host to error, got nil")
	}
	if !strings.Contains(err.Error(), "blocked redirect") {
		t.Fatalf("error %q does not mention the block", err)
	}
}

// TestCheckBundleRedirect_AllowsAllowlisted proves a redirect to an https
// allowlisted host is permitted by CheckRedirect (the guard does not block
// legitimate hops).
func TestCheckBundleRedirect_AllowsAllowlisted(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://abs.twimg.com/x.js", nil)
	if err := checkBundleRedirect(req, nil); err != nil {
		t.Fatalf("allowlisted https host should pass, got %v", err)
	}
}

func TestAllowedBundleHost(t *testing.T) {
	allowed := []string{"abs.twimg.com", "x.com", "api.twitter.com", "twitter.com"}
	for _, h := range allowed {
		if !allowedBundleHost(h) {
			t.Errorf("%q should be allowed", h)
		}
	}
	denied := []string{"169.254.169.254", "evil.com", "localhost", "abs.twimg.com.evil.com", ""}
	for _, h := range denied {
		if allowedBundleHost(h) {
			t.Errorf("%q should be denied", h)
		}
	}
}

// TestHTTPFetcher_RefusesDisallowedInitialURL proves the belt-and-suspenders
// initial-URL check rejects a non-allowlisted target before any dial.
func TestHTTPFetcher_RefusesDisallowedInitialURL(t *testing.T) {
	f, err := NewHTTPFetcher("")
	if err != nil {
		t.Fatalf("NewHTTPFetcher: %v", err)
	}
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"https://evil.com/x.js",
		"http://abs.twimg.com/x.js", // http downgrade of an allowed host
	} {
		if _, err := f.Fetch(context.Background(), u); err == nil {
			t.Fatalf("Fetch(%q) should refuse, got nil error", u)
		}
	}
}
