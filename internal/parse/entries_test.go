package parse

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bodies below are SYNTHETIC: hand-built to match the documented URT entry
// shapes (pin, module, add-to-module, replace, tombstone, retweet, note_tweet).
// Where a real capture carries the shape (note_tweet and edit_control in
// testdata/usertweets_vitalik.json) the test reads that fixture instead.

type m = map[string]any

func entUser(id, handle string) m {
	return m{"result": m{"__typename": "User", "rest_id": id,
		"core": m{"screen_name": handle, "name": handle, "created_at": "Mon Jan 02 15:04:05 +0000 2020"}}}
}

func entTweet(id, text string, extra m) m {
	t := m{"__typename": "Tweet", "rest_id": id,
		"core":   m{"user_results": entUser("U1", "alice")},
		"legacy": m{"full_text": text, "created_at": "Tue Mar 03 10:00:00 +0000 2026", "user_id_str": "U1"},
		"views":  m{"count": "42"}}
	for k, v := range extra {
		if k == "legacy" {
			for lk, lv := range v.(m) {
				t["legacy"].(m)[lk] = lv
			}
			continue
		}
		t[k] = v
	}
	return t
}

func entItemContent(result m, extra m) m {
	ic := m{"itemType": "TimelineTweet", "__typename": "TimelineTweet", "tweet_results": m{"result": result}}
	for k, v := range extra {
		ic[k] = v
	}
	return ic
}

func entEntry(id string, ic m) m {
	return m{"entryId": id, "sortIndex": "1", "content": m{"entryType": "TimelineTimelineItem", "itemContent": ic}}
}

func entModule(id string, items ...m) m {
	var its []m
	for i, ic := range items {
		// Item ids deliberately do not echo the module id, so a promoted
		// module is only recognised through its parent.
		its = append(its, m{"entryId": "item-" + string(rune('0'+i)), "item": m{"itemContent": ic}})
	}
	return m{"entryId": id, "sortIndex": "1", "content": m{"entryType": "TimelineTimelineModule", "items": its}}
}

func entBody(t *testing.T, instructions ...m) []byte {
	t.Helper()
	b, err := json.Marshal(m{"data": m{"user": m{"result": m{"timeline": m{"timeline": m{"instructions": instructions}}}}}})
	require.NoError(t, err)
	return b
}

func entAdd(entries ...m) m { return m{"type": "TimelineAddEntries", "entries": entries} }

func byID(t *testing.T, body []byte) map[string]int {
	t.Helper()
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	idx := map[string]int{}
	for i, tw := range tweets {
		idx[tw.ID] = i
	}
	return idx
}

// F3: the pinned tweet arrives in instruction.entry and used to be dropped.
func TestTweetTimeline_PinEntryKeptAndMarked(t *testing.T) {
	body := entBody(t,
		m{"type": "TimelinePinEntry", "entry": entEntry("tweet-100", entItemContent(entTweet("100", "old pinned", nil), nil))},
		entAdd(entEntry("tweet-200", entItemContent(entTweet("200", "fresh", nil), nil))))
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	require.Len(t, tweets, 2)
	assert.Equal(t, "100", tweets[0].ID)
	assert.True(t, tweets[0].Pinned, "pinned tweet must be marked so it is not read as a fresh post")
	assert.Equal(t, "200", tweets[1].ID)
	assert.False(t, tweets[1].Pinned)
}

// F3: profile conversation modules and TimelineAddToModule carry the author's
// own self-thread tweets; none of them reached the caller.
func TestTweetTimeline_ModuleItemsKept(t *testing.T) {
	body := entBody(t,
		entAdd(entModule("profile-conversation-1",
			entItemContent(entTweet("301", "thread root", nil), nil),
			entItemContent(entTweet("302", "self reply", m{"legacy": m{"in_reply_to_status_id_str": "301"}}), nil))),
		m{"type": "TimelineAddToModule", "moduleEntryId": "profile-conversation-1", "moduleItems": []m{
			{"entryId": "profile-conversation-1-item-9", "item": m{"itemContent": entItemContent(entTweet("303", "late item", nil), nil)}}}})
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	var ids []string
	for _, tw := range tweets {
		ids = append(ids, tw.ID)
	}
	require.Equal(t, []string{"301", "302", "303"}, ids)
	assert.Equal(t, "301", tweets[1].InReplyToID)
}

// F3: a TimelineReplaceEntry supersedes the entry it names; the page must not
// carry both versions.
func TestTweetTimeline_ReplaceEntrySupersedes(t *testing.T) {
	body := entBody(t,
		entAdd(entEntry("tweet-400", entItemContent(entTweet("400", "before", nil), nil))),
		m{"type": "TimelineReplaceEntry", "entry_id_to_replace": "tweet-400",
			"entry": entEntry("tweet-400", entItemContent(entTweet("400", "after", nil), nil))})
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, "after", tweets[0].Text)
}

// F3: an entry of a type the parser does not know must not break the page and
// must leave a breadcrumb.
func TestTweetTimeline_UnknownItemSkippedWithBreadcrumb(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	future := m{"itemType": "TimelineFutureThing", "__typename": "TimelineFutureThing"}
	body := entBody(t, entAdd(
		entEntry("future-1", future),
		entEntry("tweet-500", entItemContent(entTweet("500", "after unknown", nil), nil))))
	idx := byID(t, body)
	assert.Contains(t, idx, "500", "an unknown entry must not take later tweets with it")
	assert.Contains(t, buf.String(), "TimelineFutureThing", "an unhandled item type must be logged")
}

// NOT REAL pin (green on main): tombstones and unavailable results are skipped,
// not surfaced as empty tweets; a visibility wrapper is unwrapped.
func TestTweetTimeline_TombstoneSkippedWrapperUnwrapped(t *testing.T) {
	tomb := m{"__typename": "TweetTombstone", "tombstone": m{"text": m{"text": "deleted"}}}
	unavail := m{"__typename": "TweetUnavailable", "reason": "Protected"}
	wrapped := m{"__typename": "TweetWithVisibilityResults", "tweet": entTweet("610", "sensitive", nil)}
	body := entBody(t, entAdd(
		entEntry("tweet-601", entItemContent(tomb, nil)),
		entEntry("tweet-602", entItemContent(unavail, nil)),
		entEntry("tweet-610", entItemContent(wrapped, nil))))
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, "610", tweets[0].ID)
	assert.Equal(t, "sensitive", tweets[0].Text)
}

// F4 on a REAL capture: legacy.full_text is cut at ~280 characters while
// note_tweet holds the whole text; the same capture is an edited tweet.
func TestTweetTimeline_RealFixture_NoteTweetAndEdit(t *testing.T) {
	body, err := os.ReadFile("../../testdata/usertweets_vitalik.json")
	require.NoError(t, err)
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	tw := tweets[0]
	assert.Greater(t, utf8.RuneCountInString(tw.Text), 4000, "note_tweet text must win over the truncated full_text")
	assert.True(t, strings.HasPrefix(tw.Text, "This year, the EF is decreasing its budget"))
	assert.Equal(t, "2069428396661051587", tw.EditOf, "a later edit must name its first version")
}

func TestParseTweet_EditOfFirstVersionEmpty(t *testing.T) {
	first := entTweet("700", "v1", m{"edit_control": m{"edit_tweet_ids": []string{"700", "701"}}})
	later := entTweet("701", "v2", m{"edit_control": m{"initial_tweet_id": "700",
		"edit_control_initial": m{"edit_tweet_ids": []string{"700", "701"}}}})
	body := entBody(t, entAdd(entEntry("tweet-700", entItemContent(first, nil)), entEntry("tweet-701", entItemContent(later, nil))))
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	require.Len(t, tweets, 2)
	assert.Equal(t, "", tweets[0].EditOf)
	assert.Equal(t, "700", tweets[1].EditOf)
}

// F4: retweet, quote and reply markers (synthetic body).
func TestParseTweet_RetweetQuoteReplyMarkers(t *testing.T) {
	orig := entTweet("900", "RT_CUT…", m{
		"core":       m{"user_results": entUser("U2", "bob")},
		"note_tweet": m{"note_tweet_results": m{"result": m{"text": "the whole original text"}}}})
	rt := entTweet("901", "RT @bob: RT_CUT…", m{"legacy": m{"retweeted_status_result": m{"result": orig}}})
	quote := entTweet("902", "look", m{"legacy": m{"is_quote_status": true, "quoted_status_id_str": "900"},
		"quoted_status_result": m{"result": orig}})
	reply := entTweet("903", "@bob yes", m{"legacy": m{"in_reply_to_status_id_str": "900"}})
	plain := entTweet("904", "original post", nil)
	quoteOnlyResult := entTweet("905", "no id field", m{"quoted_status_result": m{"result": orig}})
	body := entBody(t, entAdd(
		entEntry("tweet-905", entItemContent(quoteOnlyResult, nil)),
		entEntry("tweet-901", entItemContent(rt, nil)), entEntry("tweet-902", entItemContent(quote, nil)),
		entEntry("tweet-903", entItemContent(reply, nil)), entEntry("tweet-904", entItemContent(plain, nil))))
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	require.Len(t, tweets, 5)
	assert.Equal(t, "900", tweets[0].QuotedID, "a quote carrying only quoted_status_result still names its target")
	tweets = tweets[1:]
	require.NotNil(t, tweets[0].RetweetOf, "retweet original must decode")

	assert.Equal(t, "900", tweets[0].RetweetOfID, "a retweet must not look like an original")
	require.NotNil(t, tweets[0].RetweetOf)
	assert.Equal(t, "bob", tweets[0].RetweetOf.AuthorHandle)
	assert.Equal(t, "RT @bob: the whole original text", tweets[0].Text, "retweet text must not be the cut form")
	assert.Equal(t, "900", tweets[1].QuotedID)
	assert.Equal(t, "900", tweets[2].InReplyToID)
	assert.Empty(t, tweets[3].RetweetOfID+tweets[3].QuotedID+tweets[3].InReplyToID+tweets[3].EditOf)
	assert.Nil(t, tweets[3].RetweetOf)
	assert.False(t, tweets[3].Pinned)
}

// F4: Promoted on every promoted shape.
func TestTweetTimeline_PromotedShapes(t *testing.T) {
	tw := func(id string) m { return entTweet(id, "x", nil) }
	body := entBody(t, entAdd(
		entEntry("promoted-tweet-1", entItemContent(tw("1"), nil)),
		entEntry("promotedTweet-2", entItemContent(tw("2"), nil)),
		entEntry("tweet-3", entItemContent(tw("3"), m{"promotedMetadata": m{"adId": "a"}})),
		entEntry("tweet-4", entItemContent(tw("4"), m{"promotedMetadata": nil})),
		entEntry("tweet-5", entItemContent(tw("5"), nil)),
		entModule("promoted-module-6", entItemContent(tw("6"), nil)),
		entEntry("tweet-promoted-7", entItemContent(tw("7"), nil)),
		entModule("profile-conversation-9", entItemContent(tw("9"), nil))),
		m{"type": "TimelineAddToModule", "moduleEntryId": "promoted-module-6", "moduleItems": []m{
			{"entryId": "item-late", "item": m{"itemContent": entItemContent(tw("8"), nil)}}}},
		m{"type": "TimelineAddToModule", "moduleEntryId": "profile-conversation-9", "moduleItems": []m{
			{"entryId": "item-late", "item": m{"itemContent": entItemContent(tw("10"), nil)}}}})
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	got := map[string]bool{}
	for _, x := range tweets {
		got[x.ID] = x.Promoted
	}
	assert.Equal(t, map[string]bool{"1": true, "2": true, "3": true, "4": false, "5": false, "6": true,
		"7": true, "8": true, "9": false, "10": false}, got)
}

// Nit: views.count as a bare JSON number used to fail the whole tweet's decode,
// silently dropping it; a non-numeric count stays 0.
func TestParseTweet_ViewsTolerant(t *testing.T) {
	num := entTweet("11", "n", m{"views": m{"count": 55}})
	str := entTweet("12", "s", m{"views": m{"count": "1.2K"}})
	none := entTweet("13", "a", m{"views": m{"state": "Enabled"}})
	ok := entTweet("14", "k", m{"views": m{"count": "147516"}})
	body := entBody(t, entAdd(entEntry("t-11", entItemContent(num, nil)), entEntry("t-12", entItemContent(str, nil)),
		entEntry("t-13", entItemContent(none, nil)), entEntry("t-14", entItemContent(ok, nil))))
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	require.Len(t, tweets, 4, "a numeric views.count must not drop the tweet")
	assert.Equal(t, []int{55, 0, 0, 147516}, []int{tweets[0].Views, tweets[1].Views, tweets[2].Views, tweets[3].Views})
}

func TestTruncateBytes_RuneBoundary(t *testing.T) {
	b := []byte(strings.Repeat("é", 30)) // 2 bytes per rune
	for n := 1; n < len(b); n++ {
		got := TruncateBytes(b, n)
		assert.True(t, utf8.ValidString(got), "n=%d produced invalid UTF-8: %q", n, got)
		assert.LessOrEqual(t, len(strings.TrimSuffix(got, "...")), n)
	}
	assert.Equal(t, "abc...", TruncateBytes([]byte("abcdef"), 3))
	assert.Equal(t, "abc", TruncateBytes([]byte("abc"), 3))
}

func TestFindStringKey_DeterministicAcrossSiblings(t *testing.T) {
	v := m{"c": m{"k": "C"}, "a": m{"k": "A"}, "b": m{"k": "B"}, "d": m{"x": m{"k": "D"}}}
	for i := 0; i < 200; i++ {
		require.Equal(t, "A", findStringKey(v, "k"), "iteration %d", i)
	}
}

// A module can carry another author's tweet; with no legacy.user_id_str the
// profile owner passed by the caller must not be stamped on it.
func TestTweetTimeline_ModuleTweetKeepsOwnAuthor(t *testing.T) {
	other := entTweet("801", "from carol", m{"core": m{"user_results": entUser("U7", "carol")}, "legacy": m{"user_id_str": ""}})
	own := entTweet("802", "from owner", m{"legacy": m{"user_id_str": ""}, "core": m{"user_results": m{"result": m{"__typename": "User"}}}})
	body := entBody(t, entAdd(entModule("profile-conversation-8",
		entItemContent(other, nil), entItemContent(own, nil))))
	tweets, err := TweetTimeline(body, "OWNER")
	require.NoError(t, err)
	require.Len(t, tweets, 2)
	assert.Equal(t, "U7", tweets[0].AuthorID)
	assert.Equal(t, "OWNER", tweets[1].AuthorID, "no author information at all falls back to the caller's id")
}

// The pinned tweet is delivered again among the entries; the page carries it
// once, first, and marked.
func TestTweetTimeline_PinnedDuplicateCollapsed(t *testing.T) {
	pinned := entTweet("100", "pinned", nil)
	body := entBody(t,
		entAdd(entEntry("tweet-200", entItemContent(entTweet("200", "fresh", nil), nil)),
			entEntry("tweet-100", entItemContent(pinned, nil))),
		m{"type": "TimelinePinEntry", "entry": entEntry("tweet-100", entItemContent(pinned, nil))})
	tweets, err := TweetTimeline(body, "")
	require.NoError(t, err)
	require.Len(t, tweets, 2)
	assert.Equal(t, "200", tweets[0].ID)
	assert.Equal(t, "100", tweets[1].ID)
	assert.True(t, tweets[1].Pinned, "the surviving copy keeps Pinned")
	assert.False(t, tweets[0].Pinned)
}

// Replacing a module entry supersedes the module's items.
func TestTweetTimeline_ReplaceModuleSupersedesItems(t *testing.T) {
	body := entBody(t,
		entAdd(entModule("profile-conversation-9",
			entItemContent(entTweet("901", "old a", nil), nil), entItemContent(entTweet("902", "old b", nil), nil))),
		m{"type": "TimelineReplaceEntry", "entry_id_to_replace": "profile-conversation-9",
			"entry": entModule("profile-conversation-9", entItemContent(entTweet("903", "new", nil), nil))})
	idx := byID(t, body)
	assert.Equal(t, map[string]int{"903": 0}, idx)
}
