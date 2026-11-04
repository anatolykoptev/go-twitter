package twitter

import (
	"net/url"
	"strings"

	stealth "github.com/anatolykoptev/go-stealth"
)

// wireGen is the generation of the request shape an account sends. What an
// account sends on the wire (header order, fetch metadata, priority) changes
// only for the generations that opt in, so live accounts on an older identity
// keep presenting exactly what they have always presented.
type wireGen uint8

const (
	// wireLegacy is the v0.8.6 shape, frozen: headers, values and order are
	// pinned byte for byte (legacy_wire_test.go). twitterHeaderOrder is its
	// order and is not to be edited.
	wireLegacy wireGen = iota
	// wireChrome is the shape of an XHR the X web client sends from current
	// desktop Chrome.
	wireChrome
)

// wireChromeProfiles are the TLS profiles whose accounts get wireChrome: the
// current desktop Chromium profiles an account without an identity is given
// (identityCandidates; Edge shares chrome_152). Every other profile, including
// chrome_131 and the other older Chrome profiles, stays on wireLegacy, so a
// profile added to go-stealth never changes what an existing account sends
// until it is listed here on purpose.
var wireChromeProfiles = map[stealth.TLSProfile]bool{
	stealth.ProfileChrome146: true,
	stealth.ProfileChrome150: true,
	stealth.ProfileChrome152: true,
	stealth.ProfileChrome156: true,
}

// wireGenFor is the one place that decides which generation a TLS profile gets.
func wireGenFor(p stealth.TLSProfile) wireGen {
	if wireChromeProfiles[p] {
		return wireChrome
	}
	return wireLegacy
}

// accountWire is the generation an account sends, decided from the profile
// STATED on the account (set by the caller, or paired with its user agent by
// ensureIdentity), never from a TLS profile the transport defaulted to: an
// account whose profile was not stated and could not be paired stays legacy.
// Like buildAccountClient it reads the account without locking; the caller has
// completed the identity.
func accountWire(acc *Account) wireGen {
	return wireGenFor(acc.Profile.TLSProfile)
}

// accountlessWire is the generation of the clients that carry no account: the
// legacy shape unless ClientConfig.ChromeWireForAccountlessClients opts in, in
// which case it follows the transport's default profile.
func accountlessWire(cfg ClientConfig) wireGen {
	if cfg.ChromeWireForAccountlessClients {
		return wireGenFor(defaultTLSProfile)
	}
	return wireLegacy
}

// wireOrderFor shapes headers (in place) for a request bc will send to rawURL
// and returns the header order to pass to the transport. Every request that
// carries twitterHeaders, guestHeaders or loginFlowHeaders goes through it. The
// generation is the one registered for bc when the Client built it; a client
// built elsewhere is legacy.
func (c *Client) wireOrderFor(bc *stealth.BrowserClient, method, rawURL string, headers map[string]string) []string {
	return c.owned.wireOf(bc).shape(method, rawURL, headers)
}

// shape prepares headers for a request of this generation and returns the order
// they go out in. The builders (twitterHeaders, guestHeaders, loginFlowHeaders)
// keep producing the v0.8.6 set; the legacy generation sends it untouched.
func (g wireGen) shape(method, rawURL string, h map[string]string) []string {
	if g != wireChrome {
		return twitterHeaderOrder
	}
	site := fetchSite(rawURL)
	h["sec-fetch-site"] = site
	h["sec-fetch-mode"] = "cors"
	h["sec-fetch-dest"] = "empty"

	// Fetch standard: a request sends Origin when it is cross-origin (CORS), and
	// otherwise only for methods other than GET and HEAD.
	if site != "same-origin" || (method != "GET" && method != "HEAD") {
		h["origin"] = "https://x.com"
	} else {
		delete(h, "origin")
	}
	if h["referer"] == "" {
		h["referer"] = "https://x.com/"
	}
	if h["accept"] == "" {
		h["accept"] = "*/*"
	}
	if h["accept-language"] == "" {
		h["accept-language"] = "en-US,en;q=0.9"
	}
	h["accept-encoding"] = chromeAcceptEncoding
	// priority is NOT set here: go-stealth's fetch mode (chromeClientOptions)
	// adds `priority: u=1, i` together with the HTTP/2 HEADERS weight 220, and
	// the pair must arrive together (Chrome never sends the priority header with
	// weight 256). Its place is in chromeHeaderOrder.

	// A logged-out page sends no x-xp-forwarded-for.
	if h["x-guest-token"] != "" {
		delete(h, "x-xp-forwarded-for")
	}
	return chromeHeaderOrder
}

// fetchSite is the Sec-Fetch-Site value of a request from the x.com page to
// rawURL.
func fetchSite(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "cross-site"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "x.com":
		return "same-origin"
	case strings.HasSuffix(host, ".x.com"):
		return "same-site"
	}
	return "cross-site"
}

// chromeAcceptEncoding is what Chrome sends; go-stealth v1.27.0 decodes zstd.
// It applies to wireChrome only: the legacy generation keeps its own
// accept-encoding without zstd.
const chromeAcceptEncoding = "gzip, deflate, br, zstd"

// chromeClientOptions are the client options every client of a wireChrome
// identity is built with; wireClientOptions is the single hook for the
// transport-level half of the shape. WithFetchMode sets the HTTP/2 HEADERS
// weight to 220 and adds `priority: u=1, i` (the pair Chrome sends for an XHR).
// Legacy profiles never receive these options.
var chromeClientOptions = []stealth.ClientOption{stealth.WithFetchMode()}

// wireClientOptions are the extra client options for a client of generation g.
func wireClientOptions(g wireGen) []stealth.ClientOption {
	if g != wireChrome {
		return nil
	}
	return chromeClientOptions
}

// chromeHeaderOrder is the order Chrome writes the headers of an XHR from x.com
// (all HTTP/2, branded Google Chrome and Edge; Chrome for Testing puts
// accept-language earlier). It is the only table of the order, so a later
// capture changes it in one place. Provenance: wire_chrome_test.go
// (oracleXHRGet/oracleXHRPost: logged-out XHRs to a host of x.com's site, real
// Chrome 156; oracleSameOriginAuthedGet: same-origin fetch carrying x.com's
// script headers, same browser).
//
// Measured positions: everything below except x-xp-forwarded-for. Chrome's
// order of script-set headers is NOT their insertion order (the fetch capture
// inserted content-type and x-csrf-token earlier than they appear), so it
// cannot be derived from x.com's script; it looks fixed per header name.
//
// PROVISIONAL: x-xp-forwarded-for. No capture sent it (logged out sends none;
// the fetch capture was not given one). It is placed right after
// x-client-transaction-id; replace once a logged-in capture exists.
//
// content-length is written by the transport for a body; listing it first
// matches the POST capture and costs nothing on a GET.
var chromeHeaderOrder = []string{
	"content-length",
	"sec-ch-ua-platform",
	"authorization",
	"x-csrf-token",
	"sec-ch-ua",
	"x-twitter-client-language",
	"sec-ch-ua-mobile",
	"x-twitter-active-user",
	"x-client-transaction-id",
	"x-xp-forwarded-for", // PROVISIONAL position, see above
	"x-guest-token",
	"x-twitter-auth-type",
	"user-agent",
	"content-type",
	"accept",
	"origin",
	"sec-fetch-site",
	"sec-fetch-mode",
	"sec-fetch-dest",
	"referer",
	"accept-encoding",
	"accept-language",
	"cookie",
	"priority",
}
