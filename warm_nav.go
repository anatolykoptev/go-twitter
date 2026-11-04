package twitter

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
)

// navTransport is the part of a go-stealth client the navigation needs.
type navTransport interface {
	// ChromeHeaders is the document-navigation header set of the client's
	// identity (user agent, client hints, accept for its Chrome major,
	// sec-fetch-*, upgrade-insecure-requests, priority: u=0, i).
	ChromeHeaders() map[string]string
	DoWithHeaderOrderCtx(ctx context.Context, method, urlStr string, headers map[string]string, body io.Reader, order []string) ([]byte, map[string]string, int, error)
}

// warmRouter is the warm-page doer of a current identity. The HTTP/2 priority
// weight is a property of a go-stealth client, and an account's own client is
// built in fetch mode (weight 220, priority u=1, i: an XHR), so the x.com pages
// the warm fetches load go through a second client of the same identity and
// proxy built without fetch mode (weight 256, u=0, i: a document navigation).
// Everything else a warm fetch loads (the abs.twimg.com scripts) stays on the
// account's own client; a script subresource is not a navigation, and no script
// capture is in the oracle, so that shape is unchanged.
//
// The two clients share the account's cookie jar (sessionJar), so the session
// cookies go out of the jar on a navigation as on an XHR, and what a page
// response sets (guest_id, the page's other cookies) reaches the account's next
// XHR, as it does between the tabs of one browser. A navigation carries no
// explicit Cookie header of its own.
type warmRouter struct {
	page  navTransport
	other warmDoer
}

// DoCtx implements warmDoer.
func (r *warmRouter) DoCtx(ctx context.Context, method, rawURL string, headers map[string]string, body io.Reader) ([]byte, map[string]string, int, error) {
	if !isDocumentURL(rawURL) {
		return r.other.DoCtx(ctx, method, rawURL, headers, body)
	}
	h, order := navigationRequest(r.page.ChromeHeaders(), method, headers)
	return r.page.DoWithHeaderOrderCtx(ctx, method, rawURL, h, body, order)
}

// isDocumentURL reports whether rawURL is an x.com page (a document), as opposed
// to a CDN script.
func isDocumentURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "x.com" || host == "twitter.com"
}

// navigationRequest builds the headers and order of a document navigation from
// the identity's navigation set (base) and what the caller supplied: only the
// cookie (and, for a form submission, the content type) is taken from it; the
// caller's accept and user agent are the generic ones of the fetcher.
//
// A GET is a navigation typed or opened by the user: sec-fetch-site none, no
// referer. A POST is the x.com/x/migrate form submitted from an x.com page:
// same-origin, with origin and referer (not captured).
func navigationRequest(base map[string]string, method string, caller map[string]string) (map[string]string, []string) {
	h := maps.Clone(base)
	for k, v := range caller {
		switch strings.ToLower(k) {
		case "cookie":
			if v != "" {
				h["cookie"] = v
			}
		case "content-type":
			if method == http.MethodPost {
				h["content-type"] = v
			}
		}
	}
	if method == http.MethodPost {
		h["sec-fetch-site"] = "same-origin"
		h["origin"] = "https://x.com"
		h["referer"] = "https://x.com/"
		return h, chromeNavigationPostOrder
	}
	return h, chromeNavigationOrder
}
