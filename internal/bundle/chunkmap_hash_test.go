package bundle

import "testing"

// A chunk hash is not fixed length: 7 hex until 2026-08, 16 since, and the
// reference accepts any alphanumeric hash. Longer and upper-case hex hashes must
// land in Hashes (not Names) and resolve to a bundle URL.
func TestParseChunkMap_HashShapes(t *testing.T) {
	cases := []struct{ name, hash string }{
		{"7 hex", "abcdef1"},
		{"16 hex", "6f4d46a32a10fdcc"},
		{"17 hex", "0123456789abcdef0"},
		{"32 hex", "0123456789abcdef0123456789abcdef"},
		{"upper-case 16 hex", "0123456789ABCDEF"},
		{"mixed case", "6f4D46a32A10fdCC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `({1:"main",20113:"ondemand.s",30:"other"})[e]||e)+"."+({1:"aa11bb22",20113:"` + tc.hash + `",30:"bb22cc33"})[e]`
			names, hashes := parseChunkMap(body)
			if got := hashes["20113"]; got != tc.hash {
				t.Fatalf("Hashes[20113] = %q, want %q", got, tc.hash)
			}
			if got, ok := names["20113"]; !ok || got != "ondemand.s" {
				t.Fatalf("Names[20113] = %q, want ondemand.s (a %d-char hash must not shadow the name)", got, len(tc.hash))
			}
			m := &Map{Names: names, Hashes: hashes}
			want := "https://abs.twimg.com/responsive-web/client-web/ondemand.s." + tc.hash + "a.js"
			if got, ok := m.BundleURL("ondemand.s"); !ok || got != want {
				t.Fatalf("BundleURL = %q (ok=%v), want %q", got, ok, want)
			}
		})
	}
}
