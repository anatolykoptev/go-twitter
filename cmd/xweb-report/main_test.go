package main

import (
	"context"
	"os"
	"strings"
	"testing"

	twitter "github.com/anatolykoptev/go-twitter"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/internal/gqlextract"
	"github.com/anatolykoptev/go-twitter/internal/xwebnative"
)

func fixtureReport(t *testing.T) string {
	t.Helper()
	f := &cacheFetcher{dir: "testdata/site", offline: true}
	md, err := generate(context.Background(), f, config{concurrency: 2, date: "2026-10-30"})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func TestGenerate_FixtureSite(t *testing.T) {
	md := fixtureReport(t)
	for _, want := range []string{
		"entry script `entry-client-logged-out-TEST1234.js`",
		"Parsed 3 Relay artifacts (2 queries, 1 mutations)",
		"| `tweetNotificationsQuery` | `" + twitter.Endpoints["tweetNotificationsQuery"].ID + "` | `tweetNotificationsQuery` | `VcNhzcrF-pRo8nSftDQThA` | 6 | new-shape |",
		"| `FavoriteTweet` | `" + twitter.Endpoints["FavoriteTweet"].ID + "` | `likeButtonFavoriteMutation` | `i9VOuSqJJf59jkZ0zVNZMQ` | 0 | same-shape |",
		"| `UserTweets` | `" + twitter.Endpoints["UserTweets"].ID + "` | `UserTweets` | `iZqjc5hRNt2d2a2l6TV2ag` | 6 | new-shape |",
		"| `Followers` | `" + twitter.Endpoints["Followers"].ID + "` | - | - | - | not-found-in-x-web |",
		// the native op's rotated id is called out; same-named legacy ops are not
		"`tweetNotificationsQuery` already runs on x-web, but the committed id",
		"`UserTweets` exists on both stacks under the same name",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q", want)
		}
	}
}

func TestGenerate_DeterministicAndNoNetworkWhenOffline(t *testing.T) {
	if a, b := fixtureReport(t), fixtureReport(t); a != b {
		t.Error("report differs between two runs on the same cache")
	}
}

func TestGenerate_EmptyCrawlIsAnErrorNotAnEmptyReport(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/home.html",
		[]byte(`<script src="https://abs.twimg.com/x-web/x-web/entry-client-logged-out-E.js"></script>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+"/x-web/x-web", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/x-web/x-web/entry-client-logged-out-E.js", []byte("export{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := generate(context.Background(), &cacheFetcher{dir: dir, offline: true}, config{concurrency: 1})
	if err == nil || !strings.Contains(err.Error(), "no Relay artifact") {
		t.Errorf("err = %v, want the no-artifact refusal", err)
	}
}

// The registry is the oracle: an Endpoints key without a spec would silently
// vanish from the report, and a spec for a removed op would rot.
func TestSpecsCoverEveryEndpoint(t *testing.T) {
	have := map[string]bool{}
	for _, s := range specs {
		if have[s.Legacy] {
			t.Errorf("duplicate spec for %s", s.Legacy)
		}
		have[s.Legacy] = true
		if _, ok := twitter.Endpoints[s.Legacy]; !ok {
			t.Errorf("spec %q has no Endpoints entry", s.Legacy)
		}
		if len(s.XWeb) == 0 {
			t.Errorf("spec %q lists no x-web candidate", s.Legacy)
		}
	}
	for name := range twitter.Endpoints {
		if !have[name] {
			t.Errorf("Endpoints[%q] has no spec: add a row to mapping.go", name)
		}
	}
}

func TestShapeVerdict(t *testing.T) {
	tests := []struct {
		name, legacy, x, want string
	}{
		{"mutation set contains legacy leaf", "create_tweet.tweet_results.result", "create_tweet.tweet_results.{result,rest_id}", verdictSame},
		{"ack", "favorite_tweet", "{favorite_tweet}", verdictSame},
		{"set lacks legacy leaf", "unretweet.source_tweet_results.result", "unretweet.source_tweet_results.{rest_id}", verdictNew},
		{"renamed root", "user.result", "user_result_by_screen_name.result.{__typename}", verdictNew},
		{"urt normalised timeline", "viewer_v2.user_results.result.tweet_notification_timeline.timeline",
			"viewer_v2.user_results.result.tweet_notification_timeline.__timeline_urt", verdictNew},
		{"unreadable artifact", "x", "", verdictUnknown},
		{"no legacy call site", "", "a.b", verdictUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := shapeVerdict(tt.legacy, tt.x); got != tt.want {
				t.Errorf("shapeVerdict(%q, %q) = %s, want %s", tt.legacy, tt.x, got, tt.want)
			}
		})
	}
}

func TestCompare_FirstCandidateWinsAndAlternativesListed(t *testing.T) {
	byName := map[string]gqlextract.Op{
		"a": {Name: "a", ID: "ida", ResponseRoot: "r"},
		"b": {Name: "b", ID: "idb", ResponseRoot: "r"},
	}
	r := compare(opSpec{Legacy: "L", XWeb: []string{"missing", "a", "b"}, LegacyRoot: "r"}, "legacy", byName)
	if r.x == nil || r.x.Name != "a" || len(r.alts) != 1 || r.alts[0] != "b" || r.verdict != verdictSame {
		t.Errorf("row = %+v", r)
	}
}

func TestCachePath(t *testing.T) {
	tests := map[string]string{
		"https://x.com/home":                                  "home.html",
		"https://abs.twimg.com/x-web/x-web/assets/a-B.js":     "x-web/x-web/assets/a-B.js",
		"https://abs.twimg.com/x-web/x-web/../../../etc/p.js": "etc/p.js",
	}
	for in, want := range tests {
		got, err := cachePath(in)
		if err != nil || got != want {
			t.Errorf("cachePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestRefusesInheritedQIDOverride(t *testing.T) {
	t.Setenv("TWITTER_QID_USER_TWEETS", "pinned")
	err := run([]string{"-cache", "testdata/site", "-offline"})
	if err == nil || !strings.Contains(err.Error(), "TWITTER_QID_USER_TWEETS") || !strings.Contains(err.Error(), "pins") {
		t.Errorf("err = %v, want a refusal naming the variable", err)
	}
}

// The refresh and x-web switches share the TWITTER_QID_ prefix but pin no id:
// they must not block a run, and the refusal must say what it is refusing.
func TestIDOverrideInEnv(t *testing.T) {
	tests := []struct {
		env  []string
		want string
	}{
		{[]string{"TWITTER_QID_REFRESH=off", "TWITTER_QID_XWEB=on", "HOME=/x"}, ""},
		{[]string{"TWITTER_QID_USER_TWEETS=abc"}, "TWITTER_QID_USER_TWEETS"},
		{[]string{"TWITTER_QID_XWEB=on", "TWITTER_QID_BOOKMARKS=abc"}, "TWITTER_QID_BOOKMARKS"},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := idOverrideInEnv(tt.env); got != tt.want {
			t.Errorf("idOverrideInEnv(%v) = %q, want %q", tt.env, got, tt.want)
		}
	}
	t.Setenv("TWITTER_QID_XWEB", "on")
	t.Setenv("TWITTER_QID_REFRESH", "off")
	if err := run([]string{"-cache", "testdata/site", "-offline", "-out", t.TempDir() + "/r.md"}); err != nil {
		t.Errorf("switch variables must not block a run: %v", err)
	}
}

// Every operation the refresher treats as x-web native must be one the report
// compares under that same Relay name, first: a legacy op added to the shared
// list (SearchTimeline's x-web artifact is SearchByRawQuery) has no artifact of
// its own name.
//
// Mutation: add "SearchTimeline" to internal/xwebnative Ops() => RED.
func TestNativeOpsAreComparedUnderTheirOwnName(t *testing.T) {
	for op := range xwebnative.Ops() {
		var found bool
		for _, s := range specs {
			if s.Legacy == op {
				found = true
				if s.XWeb[0] != op {
					t.Errorf("native op %q is compared against %q, not itself", op, s.XWeb[0])
				}
			}
		}
		if !found {
			t.Errorf("native op %q has no spec", op)
		}
	}
}

func TestParseFlags_OfflineNeedsCache(t *testing.T) {
	if _, err := parseFlags([]string{"-offline"}); err == nil {
		t.Error("-offline without -cache must be rejected")
	}
	cfg, err := parseFlags(nil)
	if err != nil || cfg.concurrency != bundle.DefaultCrawlConcurrency || cfg.out != "" {
		t.Errorf("defaults = %+v, %v", cfg, err)
	}
}
