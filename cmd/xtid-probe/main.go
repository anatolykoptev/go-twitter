// Command xtid-probe measures which x-client-transaction-id source X accepts.
//
// It sends UserTweetsAndReplies (a request X validates the id on) repeatedly
// with ids from four variants and prints only HTTP status counts per variant:
//
//	a  the algorithm before the reference-parity fix (truncated hex fraction),
//	   keys from the logged-in page
//	b  the fixed algorithm, keys from the logged-in page
//	c  the fixed algorithm, keys from the cookie-less https://x.com/i/jf/
//	d  no x-client-transaction-id header
//
// Usage:
//
//	xtid-probe -creds FILE -user SCREEN_NAME [-n 15] [-qid ID] [-pace 2s] [-user-id ID]
//
// FILE holds "auth_token=...; ct0=..." (cookie-header or one pair per line). It
// is read once and never printed. The probe prints status codes and counts
// only: no bodies, headers, ids or credentials. It stops at the first HTTP 401, 403 or 429 and says why.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	twitter "github.com/anatolykoptev/go-twitter"
	"github.com/anatolykoptev/go-twitter/cmd/xtid-probe/internal/legacyxtid"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/internal/xtidprobe"
	"github.com/anatolykoptev/go-twitter/xtid"
)

const (
	maxPerVariant = 15
	minPace       = 2 * time.Second
	operation     = "UserTweetsAndReplies"
	defaultQID    = "a6fye5SJAFWPUhE2BzoxBQ" // current on 2026-10-30
)

var (
	authRe = regexp.MustCompile(`auth_token=([^;\s"]+)`)
	ct0Re  = regexp.MustCompile(`ct0=([^;\s"]+)`)
)

type variant struct {
	name string
	desc string
	gen  func(path string) (string, bool) // id, ok; ok=false means send no header
	key  string                           // animation key; compared, never printed
	err  string                           // non-empty: variant unavailable
}

func main() {
	creds := flag.String("creds", os.Getenv("XTID_PROBE_CREDS_FILE"), "file with auth_token and ct0 (never printed)")
	user := flag.String("user", "", "target screen name")
	userID := flag.String("user-id", "", "target numeric user id (skips the lookup)")
	n := flag.Int("n", maxPerVariant, "requests per variant (max 15)")
	qid := flag.String("qid", defaultQID, "UserTweetsAndReplies query id")
	pace := flag.Duration("pace", minPace, "delay between requests (min 2s)")
	flag.Parse()
	if *creds == "" || (*user == "" && *userID == "") {
		fmt.Fprintln(os.Stderr, "usage: xtid-probe -creds FILE -user SCREEN_NAME [-n 15] [-qid ID] [-pace 2s] [-user-id ID]")
		os.Exit(2)
	}
	if *n > maxPerVariant {
		*n = maxPerVariant
	}
	if *n < 1 {
		*n = 1
	}
	if *pace < minPace {
		*pace = minPace
	}

	raw, err := os.ReadFile(*creds)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read creds file")
		os.Exit(1)
	}
	am, cm := authRe.FindSubmatch(raw), ct0Re.FindSubmatch(raw)
	if am == nil || cm == nil {
		fmt.Fprintln(os.Stderr, "creds file lacks auth_token= and ct0=")
		os.Exit(1)
	}
	authToken, ct0 := string(am[1]), string(cm[1])
	redact := strings.NewReplacer(authToken, "<redacted>", ct0, "<redacted>")

	ctx := context.Background()
	bc, err := stealth.NewClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", redact.Replace(err.Error()))
		os.Exit(1)
	}
	cookie := "auth_token=" + authToken + "; ct0=" + ct0
	acctFetch := &bundle.StealthFetcher{Client: bc, UserAgent: bundle.DefaultUserAgent, Cookie: cookie}
	guestFetch := &bundle.StealthFetcher{Client: bc, UserAgent: bundle.DefaultUserAgent}

	if *userID == "" {
		id, lerr := lookupUserID(ctx, authToken, ct0, *user)
		time.Sleep(*pace)
		if lerr != nil {
			fmt.Fprintln(os.Stderr, "user lookup failed:", redact.Replace(lerr.Error()))
			os.Exit(1)
		}
		*userID = id
	}

	variants := buildVariants(ctx, acctFetch, guestFetch, *pace, redact)
	path := "/i/api/graphql/" + *qid + "/" + operation
	reqURL := buildURL(*qid, *userID)
	headers := baseHeaders(authToken, ct0)

	counts := map[string]map[int]int{}
	for _, v := range variants {
		counts[v.name] = map[int]int{}
	}
	stopped := ""
loop:
	for r := 0; r < *n; r++ {
		for k := range variants {
			v := variants[(k+r)%len(variants)] // rotate the order each round
			if v.err != "" {
				continue
			}
			h := cloneHeaders(headers)
			if id, ok := v.gen(path); ok {
				h["x-client-transaction-id"] = id
			}
			_, _, status, derr := bc.DoCtx(ctx, "GET", reqURL, h, nil)
			if derr != nil {
				status = -1
			}
			counts[v.name][status]++
			if why := stopReason(status); why != "" {
				stopped = fmt.Sprintf("stopped on HTTP %d (variant %s, round %d): %s", status, v.name, r+1, why)
				break loop
			}
			time.Sleep(*pace)
		}
	}

	fmt.Println(keyReport(variants))
	fmt.Printf("%s, up to %d requests per variant, status code -> count (-1 = transport error)\n", operation, *n)
	for _, v := range variants {
		if v.err != "" {
			fmt.Printf("%s  %-62s UNAVAILABLE: %s\n", v.name, v.desc, v.err)
			continue
		}
		fmt.Printf("%s  %-62s %s\n", v.name, v.desc, formatCounts(counts[v.name]))
	}
	if stopped != "" {
		fmt.Println(stopped)
	}
}

func buildVariants(ctx context.Context, acct, guest bundle.Fetcher, pace time.Duration, redact *strings.Replacer) []variant {
	acctPage, acctJS, aerr := xtidprobe.FetchMaterial(ctx, acct, xtid.AuthedWarmPageURL)
	time.Sleep(pace) // same floor as the request loop
	jfPage, jfJS, jerr := xtidprobe.FetchMaterial(ctx, guest, xtid.LoggedOutPageURL)
	time.Sleep(pace)

	mk := func(name, desc string, page, js string, ferr error, legacy bool) variant {
		v := variant{name: name, desc: desc}
		if ferr != nil {
			v.err = short(redact.Replace(ferr.Error()))
			return v
		}
		if legacy {
			ct, err := legacyxtid.New(page, js)
			if err != nil {
				v.err = short(err.Error())
				return v
			}
			v.gen = func(p string) (string, bool) { return ct.GenerateID("GET", p), true }
			v.key = ct.AnimationKey()
			return v
		}
		ct, err := xtidprobe.NewTransaction(page, js)
		if err != nil {
			v.err = short(err.Error())
			return v
		}
		v.gen = func(p string) (string, bool) { return ct.GenerateID("GET", p), true }
		v.key = ct.AnimationKey()
		return v
	}
	return []variant{
		mk("a", "old algorithm (truncated hex), logged-in page", acctPage, acctJS, aerr, true),
		mk("b", "fixed algorithm, logged-in page", acctPage, acctJS, aerr, false),
		mk("c", "fixed algorithm, logged-out /i/jf/ page", jfPage, jfJS, jerr, false),
		{name: "d", desc: "no x-client-transaction-id", gen: func(string) (string, bool) { return "", false }},
	}
}

func lookupUserID(ctx context.Context, authToken, ct0, handle string) (string, error) {
	dir, err := os.MkdirTemp("", "xtid-probe-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	c, err := twitter.NewClient(twitter.ClientConfig{
		Accounts:   []*twitter.Account{{Username: "probe", AuthToken: authToken, CT0: ct0}},
		SessionDir: dir,
	})
	if err != nil {
		return "", err
	}
	u, err := c.GetUserByScreenName(ctx, handle)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

func buildURL(qid, userID string) string {
	vars, _ := json.Marshal(map[string]any{
		"userId": userID, "count": 20, "includePromotedContent": true,
		"withCommunity": true, "withVoice": true,
	})
	feats, _ := json.Marshal(xtidprobe.GraphQLFeatures())
	toggles, _ := json.Marshal(map[string]any{"withArticlePlainText": false})
	q := url.Values{}
	q.Set("variables", string(vars))
	q.Set("features", string(feats))
	q.Set("fieldToggles", string(toggles))
	return "https://x.com/i/api/graphql/" + qid + "/" + operation + "?" + q.Encode()
}

func baseHeaders(authToken, ct0 string) map[string]string {
	ua := bundle.DefaultUserAgent
	h := map[string]string{
		"authorization":             "Bearer " + twitter.BearerToken,
		"x-csrf-token":              ct0,
		"x-twitter-active-user":     "yes",
		"x-twitter-auth-type":       "OAuth2Session",
		"x-twitter-client-language": "en",
		"content-type":              "application/json",
		"cookie":                    "auth_token=" + authToken + "; ct0=" + ct0,
		"user-agent":                ua,
		"accept":                    "*/*",
		"accept-language":           "en-US,en;q=0.9",
		"accept-encoding":           "gzip, deflate, br",
		"referer":                   "https://x.com/",
		"origin":                    "https://x.com",
		"sec-fetch-dest":            "empty",
		"sec-fetch-mode":            "cors",
		"sec-fetch-site":            "same-origin",
	}
	for k, v := range stealth.ClientHintsHeaders(ua) {
		h[k] = v
	}
	return h
}

func cloneHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h)+1)
	for k, v := range h {
		out[k] = v
	}
	return out
}

// keyReport compares the animation keys of the variants without printing them.
// The old algorithm only ever drops hex fraction digits (the rest of the key is
// identical), so the length difference b-a is the number of digits it dropped.
func keyReport(vs []variant) string {
	by := map[string]variant{}
	for _, v := range vs {
		by[v.name] = v
	}
	a, b, c := by["a"], by["b"], by["c"]
	cmp := func(x, y variant) string {
		if x.err != "" || y.err != "" {
			return "n/a"
		}
		if x.key != y.key {
			return "yes"
		}
		return "no"
	}
	out := fmt.Sprintf("anim keys: a!=b %s, b!=c %s", cmp(a, b), cmp(b, c))
	if a.err == "" && b.err == "" {
		out += fmt.Sprintf("; hex fraction digits dropped by the old path: %d", len(b.key)-len(a.key))
	}
	return out
}

// stopReason says why a status ends the run ("" = keep going). Any 401/403/429
// stops it: continuing against a rejecting account or a rate limit only burns
// the account.
func stopReason(status int) string {
	switch status {
	case 429:
		return "rate limited"
	case 401, 403:
		return "credentials or session rejected; check the account before retrying"
	}
	return ""
}

func formatCounts(m map[int]int) string {
	if len(m) == 0 {
		return "no requests sent"
	}
	codes := make([]int, 0, len(m))
	for c := range m {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	parts := make([]string, len(codes))
	for i, c := range codes {
		parts[i] = fmt.Sprintf("%d x%d", c, m[c])
	}
	return strings.Join(parts, ", ")
}

func short(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
