package twitter

import (
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

// F5: the xtid warm-page fetcher carries the request account's cookie and goes
// through that account's client; guest requests get no authenticated fetcher.
func TestWarmFetcher_CarriesAccountCookie(t *testing.T) {
	bc, err := stealth.NewClient()
	if err != nil {
		t.Fatalf("stealth client: %v", err)
	}
	const cookie = "auth_token=tok123; ct0=csrf456"

	f := warmFetcher(bc, map[string]string{"cookie": cookie, "user-agent": "UA/1"})
	sf, ok := f.(*bundle.StealthFetcher)
	if !ok {
		t.Fatalf("warmFetcher = %T, want *bundle.StealthFetcher", f)
	}
	if sf.Cookie != cookie {
		t.Fatalf("warm fetcher cookie = %q, want the account cookie", sf.Cookie)
	}
	if sf.UserAgent != "UA/1" {
		t.Fatalf("warm fetcher UA = %q, want the account UA", sf.UserAgent)
	}
}

func TestWarmFetcher_NoAuthToken_NoAuthFetcher(t *testing.T) {
	bc, err := stealth.NewClient()
	if err != nil {
		t.Fatalf("stealth client: %v", err)
	}
	for name, h := range map[string]map[string]string{
		"no cookie":        {},
		"empty auth_token": {"cookie": "auth_token=; ct0=x"},
		"ct0 only":         {"cookie": "ct0=x"},
	} {
		if f := warmFetcher(bc, h); f != nil {
			t.Errorf("%s: warmFetcher = %T, want nil (unauthenticated fallback)", name, f)
		}
	}
}
