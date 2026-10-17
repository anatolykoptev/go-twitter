package main

// Authed-request helpers for the operations go-twitter does NOT expose:
// follow/unfollow (friendships/*.json), list management (GraphQL mutations
// with a v1.1 fallback), bell toggles (friendships/update.json), and the
// notifications/device_follow.json read probe.
//
// Header construction mirrors go-twitter's unexported twitterHeaders /
// twitterHeaderOrder (headers.go) — verified field-for-field against it.
// Prior art for endpoints and payloads: tweety builder.py (friendships v1.1;
// CreateList nHFMQuE4PMED1R0JTN4d-Q, ListAddMember sw71TVciw1b2nRwV6eDZNA,
// DeleteList UnN9Th1BDbeLjpgjGSpL3Q; device_follow /i/api/2/).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	stealth "github.com/anatolykoptev/go-stealth"
	twitter "github.com/anatolykoptev/go-twitter"
)

// twHeaderOrder mirrors go-twitter's twitterHeaderOrder (headers.go).
var twHeaderOrder = []string{
	"authorization",
	"content-type",
	"x-csrf-token",
	"x-twitter-active-user",
	"x-twitter-client-language",
	"x-client-transaction-id",
	"x-xp-forwarded-for",
	"sec-ch-ua",
	"sec-ch-ua-mobile",
	"sec-ch-ua-platform",
	"sec-fetch-dest",
	"sec-fetch-mode",
	"sec-fetch-site",
	"cookie",
	"user-agent",
	"accept",
	"accept-language",
	"accept-encoding",
}

// xSession is one account's own stealth client for pinned write ops. Each arm
// account gets exactly one — writes never rotate. It mirrors the per-account
// client go-twitter builds (client.go:101): same proxy, same TLS profile, same
// header order, same UA — so write requests carry the same fingerprint as the
// account's timeline requests.
type xSession struct {
	bc   *stealth.BrowserClient
	auth string
	ct0  string
	ua   string
}

// newXSession mirrors go-twitter's per-account client construction.
// twAcc is the twitter.Account the client was built with (browser profile and
// UserAgent already assigned via AssignBrowserProfile).
func newXSession(acc *socialAccount, twAcc *twitter.Account) (*xSession, error) {
	opts := []stealth.ClientOption{stealth.WithHeaderOrder(twHeaderOrder)}
	if acc.Proxy != "" {
		opts = append(opts, stealth.WithProxy(acc.Proxy))
	}
	opts = append(opts, stealth.WithProfile(twAcc.Profile.TLSProfile))
	bc, err := stealth.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	return &xSession{bc: bc, auth: acc.AuthToken, ct0: acc.CT0, ua: twAcc.UserAgent}, nil
}

// headers mirrors go-twitter's twitterHeaders with a per-request content-type.
func (x *xSession) headers(contentType string) map[string]string {
	h := map[string]string{
		"authorization":             "Bearer " + twitter.BearerToken,
		"x-csrf-token":              x.ct0,
		"x-twitter-active-user":     "yes",
		"x-twitter-auth-type":       "OAuth2Session",
		"x-twitter-client-language": "en",
		"content-type":              contentType,
		"cookie":                    "auth_token=" + x.auth + "; ct0=" + x.ct0,
		"user-agent":                x.ua,
		"accept":                    "*/*",
		"accept-language":           "en-US,en;q=0.9",
		"accept-encoding":           "gzip, deflate, br",
		"referer":                   "https://x.com/",
		"origin":                    "https://x.com",
		"sec-fetch-dest":            "empty",
		"sec-fetch-mode":            "cors",
		"sec-fetch-site":            "same-origin",
	}
	if ch := stealth.ClientHintsHeaders(x.ua); ch != nil {
		for k, v := range ch {
			h[k] = v
		}
	}
	return h
}

// adoptCT0 keeps the session csrf in step with the server's set-cookie, the
// same rotation rule go-twitter applies (extractCT0FromHeaders).
func (x *xSession) adoptCT0(respHdrs map[string]string) {
	for _, k := range []string{"set-cookie", "Set-Cookie"} {
		if v := respHdrs[k]; v != "" {
			if i := strings.Index(v, "ct0="); i >= 0 {
				rest := v[i+4:]
				if j := strings.Index(rest, ";"); j >= 0 {
					rest = rest[:j]
				}
				if len(rest) >= 8 {
					x.ct0 = rest
				}
			}
		}
	}
}

// xResult is one write/probe attempt's outcome.
type xResult struct {
	Status   int
	Body     []byte
	Codes    []int // X error codes extracted from {"errors":[{"code":N}]}
	Attempts int
}

// apiError is a terminal X-side failure worth recording (429/403/326/401 per spec).
func (r xResult) err() error {
	if r.Status >= 200 && r.Status < 300 && len(r.Codes) == 0 {
		return nil
	}
	return fmt.Errorf("x api status %d codes %v body %.200s", r.Status, r.Codes, string(r.Body))
}

func extractCodes(body []byte) []int {
	var parsed struct {
		Errors []struct {
			Code int `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return nil
	}
	codes := make([]int, 0, len(parsed.Errors))
	for _, e := range parsed.Errors {
		codes = append(codes, e.Code)
	}
	return codes
}

// do issues one request; NO retries — the caller decides (spec: on 429/403/326/401
// stop immediately, do not retry through it).
func (x *xSession) do(ctx context.Context, method, urlStr, contentType string, body io.Reader) (xResult, error) {
	body2, respHdrs, status, err := x.bc.DoWithHeaderOrderCtx(ctx, method, urlStr, x.headers(contentType), body, twHeaderOrder)
	if err != nil {
		return xResult{}, fmt.Errorf("transport: %w", err)
	}
	x.adoptCT0(respHdrs)
	return xResult{Status: status, Body: body2, Codes: extractCodes(body2), Attempts: 1}, nil
}

// formPost posts a urlencoded form to a v1.1-style endpoint.
func (x *xSession) formPost(ctx context.Context, urlStr string, form url.Values) (xResult, error) {
	return x.do(ctx, "POST", urlStr, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
}

// gqlPost posts a GraphQL mutation/query body to /i/api/graphql/<id>/<op>.
func (x *xSession) gqlPost(ctx context.Context, qid, op string, variables map[string]any, features map[string]any) (xResult, error) {
	payload := map[string]any{"variables": variables, "queryId": qid}
	if features != nil {
		payload["features"] = features
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return xResult{}, err
	}
	u := fmt.Sprintf("https://x.com/i/api/graphql/%s/%s", qid, op)
	return x.do(ctx, "POST", u, "application/json", strings.NewReader(string(raw)))
}

// --- write ops -------------------------------------------------------------

// friendshipForm is the include_* parameter set tweety sends on friendships writes.
func friendshipForm() url.Values {
	return url.Values{
		"include_profile_interstitial_type": {"1"},
		"include_blocking":                  {"1"},
		"include_blocked_by":                {"1"},
		"include_followed_by":               {"1"},
		"include_want_retweets":             {"1"},
		"include_mute_edge":                 {"1"},
		"include_can_dm":                    {"1"},
		"include_can_media_tag":             {"1"},
		"include_ext_has_nft_avatar":        {"1"},
		"include_ext_is_blue_verified":      {"1"},
		"include_ext_verified_type":         {"1"},
		"include_ext_profile_image_shape":   {"1"},
		"skip_status":                       {"1"},
	}
}

func (x *xSession) follow(ctx context.Context, userID string) (xResult, error) {
	f := friendshipForm()
	f.Set("user_id", userID)
	return x.formPost(ctx, "https://x.com/i/api/1.1/friendships/create.json", f)
}

func (x *xSession) unfollow(ctx context.Context, userID string) (xResult, error) {
	f := friendshipForm()
	f.Set("user_id", userID)
	return x.formPost(ctx, "https://x.com/i/api/1.1/friendships/destroy.json", f)
}

// bell toggles new-tweet notifications for a user the session account follows
// (tweety toggle_user_notifications: friendships/update.json, device=true/false).
func (x *xSession) bell(ctx context.Context, userID string, on bool) (xResult, error) {
	f := friendshipForm()
	f.Set("id", userID)
	f.Set("cursor", "-1")
	if on {
		f.Set("device", "true")
	} else {
		f.Set("device", "false")
	}
	return x.formPost(ctx, "https://x.com/i/api/1.1/friendships/update.json", f)
}

// listGraphQLFeatures is the reduced feature set tweety sends on list mutations.
var listGraphQLFeatures = map[string]any{
	"responsive_web_graphql_exclude_directive_enabled":                  true,
	"verified_phone_label_enabled":                                      true,
	"responsive_web_graphql_skip_user_profile_image_extensions_enabled": false,
	"responsive_web_graphql_timeline_navigation_enabled":                true,
}

// ownedList is one list the session account owns (lists/ownerships).
type ownedList struct {
	ID      int64
	Name    string
	Mode    string
	Members int
}

// ownedLists GETs lists/ownerships for the session account.
func (x *xSession) ownedLists(ctx context.Context, screenName string) ([]ownedList, error) {
	q := url.Values{"screen_name": {screenName}, "count": {"100"}}
	res, err := x.do(ctx, "GET", "https://x.com/i/api/1.1/lists/ownerships.json?"+q.Encode(),
		"application/x-www-form-urlencoded", nil)
	if err != nil {
		return nil, err
	}
	if res.Status != 200 {
		return nil, res.err()
	}
	var parsed struct {
		Lists []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Mode    string `json:"mode"`
			Members int    `json:"member_count"`
		} `json:"lists"`
	}
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return nil, err
	}
	out := make([]ownedList, 0, len(parsed.Lists))
	for _, l := range parsed.Lists {
		out = append(out, ownedList{ID: l.ID, Name: l.Name, Mode: l.Mode, Members: l.Members})
	}
	return out, nil
}

// findOwnedList returns the ID of an owned list named name, or "".
func (x *xSession) findOwnedList(ctx context.Context, screenName, name string) (string, error) {
	lists, err := x.ownedLists(ctx, screenName)
	if err != nil {
		return "", err
	}
	for _, l := range lists {
		if l.Name == name {
			return fmt.Sprintf("%d", l.ID), nil
		}
	}
	return "", nil
}

// listCreate makes a private list; returns its ID. GraphQL CreateList first
// (tweety queryId), falling back to the v1.1 lists/create.json form endpoint.
// Dedupes by name before AND after GraphQL: the dry run showed GraphQL can
// create the list yet return a body digListID cannot parse — a blind v1.1
// fallback then creates a second, unrecorded list.
func (x *xSession) listCreate(ctx context.Context, ownerScreen, name string) (string, error) {
	if id, err := x.findOwnedList(ctx, ownerScreen, name); err == nil && id != "" {
		slog.Info("list already exists; reusing", "id", id)
		return id, nil
	}
	res, err := x.gqlPost(ctx, "nHFMQuE4PMED1R0JTN4d-Q", "CreateList",
		map[string]any{"isPrivate": true, "name": name, "description": "freshness-spike measurement list"},
		listGraphQLFeatures)
	if err != nil {
		return "", err
	}
	if res.Status == 200 && len(res.Codes) == 0 {
		if id := digListID(res.Body); id != "" {
			return id, nil
		}
	}
	if res.Status == 200 {
		// Ambiguous: the list may exist server-side. Re-check before writing.
		if id, lerr := x.findOwnedList(ctx, ownerScreen, name); lerr == nil && id != "" {
			slog.Info("graphql create returned 200 without parseable id; recovered via ownerships", "id", id)
			return id, nil
		}
	}
	// Fallback: legacy v1.1 endpoint.
	f := url.Values{"name": {name}, "mode": {"private"}, "description": {"freshness-spike"}}
	res2, err := x.formPost(ctx, "https://x.com/i/api/1.1/lists/create.json", f)
	if err != nil {
		return "", err
	}
	if res2.Status == 200 && len(res2.Codes) == 0 {
		if id := digListID(res2.Body); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("list create failed: graphql %s; v1.1 %s",
		res.err(), res2.err())
}

// digListID finds the created list's id in either response shape
// (GraphQL data.list.id_str / data.list.id, or v1.1 top-level id_str).
func digListID(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	var walk func(n any) string
	walk = func(n any) string {
		switch t := n.(type) {
		case map[string]any:
			for _, k := range []string{"id_str", "id"} {
				if s, ok := t[k].(string); ok && len(s) >= 5 && allDigits(s) {
					return s
				}
				if f, ok := t[k].(float64); ok && f > 1e5 {
					return fmt.Sprintf("%.0f", f)
				}
			}
			for _, v2 := range t {
				if r := walk(v2); r != "" {
					return r
				}
			}
		case []any:
			for _, v2 := range t {
				if r := walk(v2); r != "" {
					return r
				}
			}
		}
		return ""
	}
	return walk(v)
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// listAdd adds one member. GraphQL ListAddMember first, v1.1 members/create as fallback.
func (x *xSession) listAdd(ctx context.Context, listID, userID string) (xResult, error) {
	res, err := x.gqlPost(ctx, "sw71TVciw1b2nRwV6eDZNA", "ListAddMember",
		map[string]any{"listId": listID, "userId": userID}, listGraphQLFeatures)
	if err != nil {
		return xResult{}, err
	}
	if res.Status == 200 && len(res.Codes) == 0 {
		return res, nil
	}
	f := url.Values{"list_id": {listID}, "user_id": {userID}}
	res2, err := x.formPost(ctx, "https://x.com/i/api/1.1/lists/members/create.json", f)
	if err != nil {
		return xResult{}, err
	}
	if res2.Status == 200 && len(res2.Codes) == 0 {
		return res2, nil
	}
	return res, fmt.Errorf("list add failed: graphql %s; v1.1 %s", res.err(), res2.err())
}

// listDelete removes the list. GraphQL DeleteList first, v1.1 lists/destroy as fallback.
func (x *xSession) listDelete(ctx context.Context, listID string) error {
	res, err := x.gqlPost(ctx, "UnN9Th1BDbeLjpgjGSpL3Q", "DeleteList",
		map[string]any{"listId": listID}, nil)
	if err == nil && res.Status == 200 && len(res.Codes) == 0 {
		return nil
	}
	f := url.Values{"list_id": {listID}}
	res2, err2 := x.formPost(ctx, "https://x.com/i/api/1.1/lists/destroy.json", f)
	if err2 != nil {
		return err2
	}
	if res2.Status == 200 && len(res2.Codes) == 0 {
		return nil
	}
	return fmt.Errorf("list delete failed: graphql %s; v1.1 %s", res.err(), res2.err())
}

// friendsIDs returns the IDs the session account follows via the legacy
// friends/ids endpoint — one request instead of paginated GraphQL Following
// (which in the dry run burned 5 min of internal retries and then failed).
func (x *xSession) friendsIDs(ctx context.Context, userID string, count int) ([]string, error) {
	q := url.Values{
		"user_id":       {userID},
		"count":         {fmt.Sprint(count)},
		"stringify_ids": {"true"},
		"cursor":        {"-1"},
	}
	res, err := x.do(ctx, "GET", "https://x.com/i/api/1.1/friends/ids.json?"+q.Encode(),
		"application/x-www-form-urlencoded", nil)
	if err != nil {
		return nil, err
	}
	if res.Status != 200 {
		return nil, res.err()
	}
	var parsed struct {
		IDs        []string `json:"ids"`
		NextCursor int64    `json:"next_cursor"`
	}
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return nil, fmt.Errorf("friends/ids parse: %w", err)
	}
	return parsed.IDs, nil
}

// deviceFollowURL is the read probe for arm D (tweety
// get_new_user_tweet_notification). Returns tweets from users whose bells the
// session account enabled, in notification order.
const deviceFollowURL = "https://x.com/i/api/2/notifications/device_follow.json"

func (x *xSession) deviceFollowGet(ctx context.Context, count int) (xResult, error) {
	q := url.Values{
		"count":            {fmt.Sprint(count)},
		"skip_status":      {"1"},
		"tweet_mode":       {"extended"},
		"include_entities": {"true"},
		"send_error_codes": {"true"},
	}
	return x.do(ctx, "GET", deviceFollowURL+"?"+q.Encode(), "application/x-www-form-urlencoded", nil)
}

// deviceFollowTweets extracts (tweetID, screen_name) pairs from a
// device_follow response. The response nests tweet objects under different
// roots across versions ("globalObjects.tweets", "notifications[].tweet",
// "timeline.instructions"); a generic JSON walk finds every object that looks
// like a tweet (has id_str AND a user/screen_name sibling).
func deviceFollowTweets(body []byte) []obsTweet {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	var out []obsTweet
	var walk func(n any)
	walk = func(n any) {
		m, ok := n.(map[string]any)
		if !ok {
			if arr, ok := n.([]any); ok {
				for _, e := range arr {
					walk(e)
				}
			}
			return
		}
		idStr, _ := m["id_str"].(string)
		if idStr == "" {
			if f, ok := m["id"].(float64); ok && f > 1e14 {
				idStr = fmt.Sprintf("%.0f", f)
			}
		}
		var screen string
		if u, ok := m["user"].(map[string]any); ok {
			screen, _ = u["screen_name"].(string)
		}
		if screen == "" {
			screen, _ = m["screen_name"].(string)
		}
		if idStr != "" && allDigits(idStr) && screen != "" {
			out = append(out, obsTweet{ID: idStr, Handle: screen})
			return // tweet objects are leaves for our purposes
		}
		for _, v2 := range m {
			walk(v2)
		}
	}
	walk(v)
	return out
}

// noRelogin implements twitter.AutoReloginGate — the probe NEVER relogins: a
// fresh login mints a new auth_token, which can invalidate the DB-stored
// credentials that production consumers (go-search via go-social) hold.
type noRelogin struct{}

func (noRelogin) Allowed(context.Context, string) (bool, string) {
	return false, "spike: relogin disabled (would invalidate production creds)"
}

// memSessionStore is an in-memory-only SessionStore: keeps ct0 rotations for
// the process lifetime, writes nothing to disk. Safe for concurrent use per
// the SessionStore contract.
type memSessionStore struct {
	mu sync.Mutex
	m  map[string]twitter.Session
}

func newMemSessionStore() *memSessionStore {
	return &memSessionStore{m: map[string]twitter.Session{}}
}

func (s *memSessionStore) Load(_ context.Context, username string) (twitter.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.m[username]; ok {
		return sess, nil
	}
	return twitter.Session{}, twitter.ErrSessionNotFound
}

func (s *memSessionStore) Save(_ context.Context, username string, sess twitter.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[username] = sess
	return nil
}

func (s *memSessionStore) Delete(_ context.Context, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, username)
	return nil
}
