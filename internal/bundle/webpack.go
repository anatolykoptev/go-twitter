package bundle

import (
	"net/url"
	"regexp"
	"sort"
)

// webpackAttrRe matches any src/href whose value is a legacy webpack bundle:
// .../responsive-web/client-web/<name>.<hash>a.js, absolute, root-relative or
// protocol-relative.
var webpackAttrRe = regexp.MustCompile(`(?:src|href)=["']([^"'\s]*/responsive-web/client-web/[^"'\s]*\.js)["']`)

// MapFromPage reassembles the webpack chunk map embedded in a warm page body.
// The result has no entries when the page carries none (a Vite page).
func MapFromPage(page []byte) *Map {
	names, hashes := parseChunkMap(string(page))
	return &Map{Names: names, Hashes: hashes}
}

// StartURLs resolves every module name in the chunk map to its bundle URL,
// sorted for a deterministic walk order. Shared by gql-sync (offline codegen)
// and the runtime queryId refresh so both start from the same coverage.
func (m *Map) StartURLs() []string {
	urls := make([]string, 0, len(m.Names))
	for _, name := range m.Names {
		if u, ok := m.BundleURL(name); ok {
			urls = append(urls, u)
		}
	}
	sort.Strings(urls)
	return urls
}

// WebpackScripts returns the start bundles of a legacy webpack-layout page: the
// responsive-web/client-web script links the page references (main.<hash>a.js
// first, in page order), then every bundle the page's inline chunk map names.
// Links are resolved against pageURL and restricted to https + allowlisted hosts
// (page HTML is third-party data); chunk-map URLs are built on the fixed CDN
// template so they cannot leave abs.twimg.com.
func WebpackScripts(page, pageURL string) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		base = &url.URL{Scheme: "https", Host: "x.com"}
	}
	var out []string
	seen := map[string]bool{}
	add := func(abs string) {
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	for _, m := range webpackAttrRe.FindAllStringSubmatch(page, -1) {
		rel, perr := url.Parse(jsEscapes.Replace(m[1]))
		if perr != nil {
			continue
		}
		u := base.ResolveReference(rel)
		if u.Scheme != "https" || !allowedBundleHost(u.Hostname()) {
			continue
		}
		add(u.String())
	}
	for _, u := range MapFromPage([]byte(page)).StartURLs() {
		add(u)
	}
	return out
}
