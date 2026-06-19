package bundle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

// stubDoer is an in-memory stealthDoer for StealthFetcher tests.
type stubDoer struct {
	body    []byte
	status  int
	err     error
	gotURL  string
	gotUA   string
	gotVerb string
}

func (s *stubDoer) DoCtx(_ context.Context, method, urlStr string, headers map[string]string, _ io.Reader) ([]byte, map[string]string, int, error) {
	s.gotVerb = method
	s.gotURL = urlStr
	s.gotUA = headers["User-Agent"]
	if s.err != nil {
		return nil, nil, 0, s.err
	}
	return s.body, nil, s.status, nil
}

func TestStealthFetcher_Fetch(t *testing.T) {
	doer := &stubDoer{body: []byte("hello-bundle"), status: http.StatusOK}
	f := &StealthFetcher{Client: doer, UserAgent: "ua-x"}

	body, err := f.Fetch(context.Background(), "https://x.com/home")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(body) != "hello-bundle" {
		t.Fatalf("body = %q want hello-bundle", body)
	}
	if doer.gotVerb != http.MethodGet {
		t.Fatalf("method = %q want GET", doer.gotVerb)
	}
	if doer.gotURL != "https://x.com/home" {
		t.Fatalf("url = %q", doer.gotURL)
	}
	if doer.gotUA != "ua-x" {
		t.Fatalf("UA = %q want ua-x", doer.gotUA)
	}
}

func TestStealthFetcher_Non200(t *testing.T) {
	doer := &stubDoer{body: []byte("nope"), status: http.StatusForbidden}
	f := &StealthFetcher{Client: doer}

	if _, err := f.Fetch(context.Background(), "https://x.com"); err == nil {
		t.Fatal("expected an error on HTTP 403")
	}
}

func TestStealthFetcher_TransportError(t *testing.T) {
	doer := &stubDoer{err: errors.New("boom")}
	f := &StealthFetcher{Client: doer}

	if _, err := f.Fetch(context.Background(), "https://x.com"); err == nil {
		t.Fatal("expected an error on transport failure")
	}
}

func TestStealthFetcher_DefaultUA(t *testing.T) {
	doer := &stubDoer{body: []byte("ok"), status: http.StatusOK}
	f := &StealthFetcher{Client: doer} // empty UserAgent -> default

	if _, err := f.Fetch(context.Background(), "https://x.com"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doer.gotUA != defaultUserAgent {
		t.Fatalf("UA = %q want the default", doer.gotUA)
	}
}
