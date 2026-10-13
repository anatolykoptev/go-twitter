package main

import (
	"testing"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

func TestBuildFetcher_CookiesFromEnv(t *testing.T) {
	t.Setenv(cookiesEnv, "auth_token=x; ct0=y")
	f, _, err := buildFetcher(config{})
	if err != nil {
		t.Fatal(err)
	}
	hf, ok := f.(*bundle.HTTPFetcher)
	if !ok || hf.Cookie != "auth_token=x; ct0=y" {
		t.Fatalf("fetcher = %#v, want HTTPFetcher carrying the env cookie", f)
	}

	t.Setenv(cookiesEnv, "")
	f, _, err = buildFetcher(config{})
	if err != nil {
		t.Fatal(err)
	}
	if hf, _ := f.(*bundle.HTTPFetcher); hf.Cookie != "" {
		t.Fatal("cookie set although env is empty")
	}
}
