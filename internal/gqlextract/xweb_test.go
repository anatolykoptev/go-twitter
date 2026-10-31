package gqlextract

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readXWebFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "xweb", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func opByName(t *testing.T, ops []Op, name string) Op {
	t.Helper()
	for _, o := range ops {
		if o.Name == name {
			return o
		}
	}
	t.Fatalf("op %q not found in %v", name, ops)
	return Op{}
}

func TestParseRelayArtifacts_TweetNotifications(t *testing.T) {
	ops := ParseRelayArtifacts("https://abs.twimg.com/x-web/x-web/assets/tweet-notifications-X.js",
		readXWebFixture(t, "tweet-notifications.js"))
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	got := ops[0]
	wantProviders := []string{
		"__relay_internal__pv__xclientsfeaturesappviewerisloggedinrelayprovider",
		"__relay_internal__pv__xclientsfeaturestranslationgroktranslatedpostenabledrelayprovider",
		"__relay_internal__pv__xclientsfeaturestweetcomponentsgrokanalyzepostrelayprovider",
		"__relay_internal__pv__xclientsfeaturestranslationnoteautotranslationenabledrelayprovider",
		"__relay_internal__pv__liveeventpostcontextenabledrelayprovider",
		"__relay_internal__pv__sportspostcontextenabledrelayprovider",
	}
	if got.Name != "tweetNotificationsQuery" || got.ID != "VcNhzcrF-pRo8nSftDQThA" ||
		got.Kind != "query" || got.Stack != StackXWeb {
		t.Errorf("identity = %+v", got)
	}
	if !reflect.DeepEqual(got.Variables, []string{"count", "cursor"}) {
		t.Errorf("variables = %v", got.Variables)
	}
	if !reflect.DeepEqual(got.Providers, wantProviders) {
		t.Errorf("providers = %v", got.Providers)
	}
	if got.Features != nil {
		t.Errorf("x-web artifacts declare no features, got %v", got.Features)
	}
	if want := "viewer_v2.user_results.result.tweet_notification_timeline.__timeline_urt"; got.ResponseRoot != want {
		t.Errorf("response root = %q, want %q", got.ResponseRoot, want)
	}
}

func TestParseRelayArtifacts_Fixtures(t *testing.T) {
	tests := []struct {
		file, name, id, kind string
		vars                 []string
		providers            int
		root                 string
	}{
		// shared selections and argument defs referenced by minified identifiers
		{"like-mutation.js", "likeButtonFavoriteMutation", "i9VOuSqJJf59jkZ0zVNZMQ", "mutation",
			[]string{"tweetId", "engagementRequest"}, 0, "{favorite_tweet}"},
		{"user-tweets.js", "UserTweets", "iZqjc5hRNt2d2a2l6TV2ag", "query",
			[]string{"screenName", "count", "cursor", "sortByMostLiked"}, 6,
			"user_result_by_screen_name.result.profile_with_replies_timeline_v2.__timeline_urt"},
		{"home-latest.js", "HomeLatestTimelineQuery", "A2Mz89dkCmyP-0QZdWv6nQ", "query",
			[]string{"count", "cursor", "enableRanking", "requestContext", "seenTweetIds"}, 6,
			"home_latest_timeline.__home_timeline_urt_urt"},
		{"followers.js", "followersQuery", "o0WH4pn2DlVABYq1-PYq_w", "query",
			[]string{"screenName", "count", "cursor"}, 6,
			"user_result_by_screen_name.result.followers_timeline.__timeline_urt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := opByName(t, ParseRelayArtifacts("u", readXWebFixture(t, tt.file)), tt.name)
			if got.ID != tt.id || got.Kind != tt.kind {
				t.Errorf("id/kind = %s/%s, want %s/%s", got.ID, got.Kind, tt.id, tt.kind)
			}
			if !reflect.DeepEqual(got.Variables, tt.vars) {
				t.Errorf("variables = %v, want %v", got.Variables, tt.vars)
			}
			if len(got.Providers) != tt.providers {
				t.Errorf("providers = %d, want %d", len(got.Providers), tt.providers)
			}
			if got.ResponseRoot != tt.root {
				t.Errorf("root = %q, want %q", got.ResponseRoot, tt.root)
			}
		})
	}
}

func TestParseRelayArtifacts_Shapes(t *testing.T) {
	tests := []struct {
		name, body string
		want       []string // "name:id" per op
	}{
		{"no artifact", "var a=1;export{a};", nil},
		{"router params are not artifacts", "navigate({params:{id:`requests`},search:t})", nil},
		{"key order and nested metadata",
			"x=(function(){return{fragment:{argumentDefinitions:[],kind:`Fragment`,name:`A`,selections:[]},kind:`Request`," +
				"operation:{argumentDefinitions:[],kind:`Operation`,name:`A`,selections:[]}," +
				"params:{metadata:{a:{b:1}},operationKind:`query`,name:`A`,id:`idA_-1`,text:null}}})();",
			[]string{"A:idA_-1"}},
		{"quoted strings",
			`x={params:{id:"idQ",metadata:{},name:"Q",operationKind:"mutation",text:null}}`,
			[]string{"Q:idQ"}},
		{"id outside the alphabet is dropped",
			"x={params:{id:`a/b`,metadata:{},name:`Bad`,operationKind:`query`,text:null}}", nil},
		{"unbalanced is skipped, not fatal",
			"x={params:{id:`ok`,metadata:{name:`U`", nil},
		{"two artifacts in one chunk",
			"a={params:{id:`i1`,metadata:{},name:`One`,operationKind:`query`,text:null}};" +
				"b={params:{id:`i2`,metadata:{},name:`Two`,operationKind:`query`,text:null}};",
			[]string{"One:i1", "Two:i2"}},
		{"aliased inline fragment before params does not become the artifact",
			"var z=(function(){var t={defaultValue:null,kind:`LocalArgument`,name:`screenName`};" +
				"return{fragment:{argumentDefinitions:[t],kind:`Fragment`,name:`U`,selections:[{alias:null,args:null,kind:`LinkedField`,name:`user_result_by_screen_name`,plural:!1," +
				"selections:[{fragment:{kind:`InlineFragment`,selections:[],type:`User`},kind:`AliasedInlineFragmentSpread`,name:`user`}],storageKey:null}],type:`Query`}," +
				"kind:`Request`,operation:{argumentDefinitions:[t],kind:`Operation`,name:`U`,selections:[]}," +
				"params:{id:`idU`,metadata:{},name:`U`,operationKind:`query`,text:null}}})();",
			[]string{"U:idU"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, o := range ParseRelayArtifacts("u", []byte(tt.body)) {
				got = append(got, o.Name+":"+o.ID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRelayArtifacts_AliasedFragmentKeepsVariables(t *testing.T) {
	body := "var z=(function(){var t={defaultValue:null,kind:`LocalArgument`,name:`screenName`};" +
		"return{fragment:{argumentDefinitions:[t],kind:`Fragment`,name:`U`,selections:[{alias:null,args:null,kind:`LinkedField`,name:`user_result_by_screen_name`,plural:!1," +
		"selections:[{fragment:{kind:`InlineFragment`,selections:[],type:`User`},kind:`AliasedInlineFragmentSpread`,name:`user`}],storageKey:null}],type:`Query`}," +
		"kind:`Request`,operation:{argumentDefinitions:[t],kind:`Operation`,name:`U`,selections:[]}," +
		"params:{id:`idU`,metadata:{},name:`U`,operationKind:`query`,text:null}}})();"
	op := ParseRelayArtifacts("u", []byte(body))[0]
	if !reflect.DeepEqual(op.Variables, []string{"screenName"}) {
		t.Errorf("variables = %v, want [screenName]", op.Variables)
	}
	if op.ResponseRoot != "user_result_by_screen_name" {
		t.Errorf("root = %q", op.ResponseRoot)
	}
}

// The artifact's own declaration wins over an earlier same-named minified
// variable from another scope in the chunk.
func TestParseRelayArtifacts_NearestDeclarationWins(t *testing.T) {
	body := "var t=[1,2,3];var z=(function(){var t={defaultValue:null,kind:`LocalArgument`,name:`wanted`};" +
		"return{fragment:{argumentDefinitions:[t],kind:`Fragment`,name:`N`,selections:[]},kind:`Request`," +
		"operation:{argumentDefinitions:[t],kind:`Operation`,name:`N`,selections:[]}," +
		"params:{id:`idN`,metadata:{},name:`N`,operationKind:`query`,text:null}}})();"
	op := ParseRelayArtifacts("u", []byte(body))[0]
	if !reflect.DeepEqual(op.Variables, []string{"wanted"}) {
		t.Errorf("variables = %v, want [wanted]", op.Variables)
	}
}

func TestXWebExtractor_DedupeMergeConflict(t *testing.T) {
	x := NewXWebExtractor(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	paramsOnly := "p={params:{id:`idN`,metadata:{},name:`N`,operationKind:`query`,text:null}}"
	full := "var z=(function(){var t={defaultValue:null,kind:`LocalArgument`,name:`count`};" +
		"return{fragment:{argumentDefinitions:[t],kind:`Fragment`,name:`N`,selections:[{alias:null,args:null,kind:`LinkedField`,name:`root`,plural:!1,selections:[],storageKey:null}],type:`Query`}," +
		"kind:`Request`,operation:{argumentDefinitions:[t],kind:`Operation`,name:`N`,selections:[]}," +
		"params:{id:`idN`,metadata:{},name:`N`,operationKind:`query`,text:null}}})();"
	x.Consume("a", []byte(paramsOnly))
	x.Consume("b", []byte(full))
	x.Consume("c", []byte("q={params:{id:`otherID`,metadata:{},name:`N`,operationKind:`query`,text:null}}"))
	ops := x.Ops()
	if len(ops) != 1 || ops[0].ID != "idN" {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].ResponseRoot != "root" || !reflect.DeepEqual(ops[0].Variables, []string{"count"}) {
		t.Errorf("params-only chunk must be completed by the full artifact: %+v", ops[0])
	}
	if got := x.Conflicts()["N"]; !reflect.DeepEqual(got, []string{"otherID"}) {
		t.Errorf("conflicts = %v", got)
	}
}

// The legacy extractor must keep ignoring Relay artifacts: if it ever started
// reading them, an x-web id could silently override a working legacy id in
// queryids_gen.go.
func TestLegacyExtractorIgnoresRelayArtifacts(t *testing.T) {
	e := NewExtractor(nil)
	for _, f := range []string{"tweet-notifications.js", "user-tweets.js", "like-mutation.js"} {
		e.Consume("https://abs.twimg.com/x-web/x-web/assets/"+f, readXWebFixture(t, f))
	}
	if got := e.Result(); len(got) != 0 {
		t.Errorf("legacy extractor read x-web artifacts: %v", got)
	}
}

func TestExtractorOpsTagsLegacy(t *testing.T) {
	e := NewExtractor(nil)
	e.Consume("https://abs.twimg.com/responsive-web/client-web/main.js",
		[]byte(`a={queryId:"abc123",operationName:"UserTweets"}`))
	ops := e.Ops()
	if len(ops) != 1 || ops[0].Stack != StackLegacy || ops[0].Name != "UserTweets" || ops[0].ID != "abc123" {
		t.Errorf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Source, "responsive-web") {
		t.Errorf("source = %q", ops[0].Source)
	}
}
