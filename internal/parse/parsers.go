// Package parse decodes X API response bodies into the model types. It holds no
// transport, account or metrics state: every function takes the raw body and
// returns a value or an error.
package parse

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anatolykoptev/go-twitter/model"
)

// TruncateBytes returns b as a string cut to at most n bytes with a "..."
// marker. The cut backs off to a rune boundary, so valid UTF-8 input never
// yields a string ending in half a rune.
func TruncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	return string(b[:n]) + "..."
}

var tokenMentionRe = regexp.MustCompile(`\$([A-Z]{2,10})`)

// UserByScreenName parses the UserByScreenName GraphQL response.
func UserByScreenName(body []byte) (*model.TwitterUser, error) {
	var raw struct {
		Data struct {
			User struct {
				Result userResult `json:"result"`
			} `json:"user"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal UserByScreenName: %w", err)
	}
	// Data wins over errors[]: a suspended handle can arrive as a partial
	// response with both a UserUnavailable result and errors[] — the typed
	// result yields the deterministic ErrNotFound, errors[] alone is generic.
	if raw.Data.User.Result.TypeName == "" && len(raw.Errors) > 0 {
		return nil, fmt.Errorf("twitter API error: %s", raw.Errors[0].Message)
	}
	if len(raw.Errors) > 0 {
		slog.Debug("UserByScreenName errors[] alongside data", slog.String("first", raw.Errors[0].Message))
	}
	return parseUserResult(raw.Data.User.Result)
}

// UserList parses Followers/Following response.
func UserList(body []byte) ([]*model.TwitterUser, string, error) {
	pg, err := UserListPage(body)
	return pg.Users, pg.Cursor, err
}

// UserPage is one parsed page of a user list. Entries counts the TimelineUser
// entries the page carried, including ones whose profile could not be parsed
// (suspended or unavailable accounts); Users holds only the parsed ones. A page
// with Entries == 0 is empty: X has nothing more to return, whatever Cursor says.
type UserPage struct {
	Users   []*model.TwitterUser
	Cursor  string
	Entries int
}

// UserListPage parses a Followers/Following response like UserList and also
// reports how many user entries the page carried, so a paginating caller can
// tell an empty page from one whose users were all unavailable.
func UserListPage(body []byte) (UserPage, error) {
	var raw struct {
		Data struct {
			User struct {
				Result struct {
					Timeline struct {
						Timeline timelineObj `json:"timeline"`
					} `json:"timeline"`
				} `json:"result"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return UserPage{}, fmt.Errorf("unmarshal user list: %w", err)
	}
	return extractUserPage(raw.Data.User.Result.Timeline.Timeline), nil
}

// RetweeterList parses Retweeters response.
func RetweeterList(body []byte) ([]*model.TwitterUser, string, error) {
	pg, err := RetweeterListPage(body)
	return pg.Users, pg.Cursor, err
}

// RetweeterListPage parses a Retweeters response like RetweeterList and also
// reports the page's user-entry count; see UserPage.
func RetweeterListPage(body []byte) (UserPage, error) {
	var raw struct {
		Data struct {
			RetweetersTimeline struct {
				Timeline timelineObj `json:"timeline"`
			} `json:"retweeters_timeline"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return UserPage{}, fmt.Errorf("unmarshal retweeter list: %w", err)
	}
	tl := raw.Data.RetweetersTimeline.Timeline
	if len(tl.Instructions) == 0 {
		return UserListPage(body)
	}
	return extractUserPage(tl), nil
}

// TweetDetail parses a TweetDetail GraphQL response. focalID is the
// requested tweet: when non-empty and absent from the extracted page the
// result is a deterministic miss — ErrNotFound, not a silently substituted
// tweets[0] (a deleted focal still returns ancestors/replies that would
// otherwise be misidentified as the requested tweet).
func TweetDetail(body []byte, focalID string, opts ...Option) ([]*model.Tweet, error) {
	type itemContentRef = json.RawMessage
	type moduleItem struct {
		EntryID string `json:"entryId"`
		Item    struct {
			ItemContent itemContentRef `json:"itemContent"`
		} `json:"item"`
	}
	type convEntry struct {
		EntryID string `json:"entryId"`
		Content struct {
			ItemContent itemContentRef `json:"itemContent"`
			// conversationthread-* modules carry replies/ancestors nested
			// under items[].item.itemContent — the live TweetDetail shape.
			Items []moduleItem `json:"items"`
		} `json:"content"`
	}
	type conversationData struct {
		Instructions []struct {
			Type    string      `json:"type"`
			Entries []convEntry `json:"entries"`
			Entry   *convEntry  `json:"entry"` // TimelinePinEntry delivers the focal tweet here
		} `json:"instructions"`
	}
	var raw struct {
		Data struct {
			// Twitter uses both keys depending on the endpoint version
			V2 conversationData `json:"threaded_conversation_with_injections_v2"`
			V1 conversationData `json:"threaded_conversation_with_injections"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal TweetDetail: %w", err)
	}
	// Use v2 if it has instructions, otherwise fall back to v1
	conv := raw.Data.V2
	if len(conv.Instructions) == 0 {
		conv = raw.Data.V1
	}
	// errors[] is fatal only when no usable conversation data arrived —
	// X routinely ships partial pages alongside non-fatal GraphQL errors.
	if len(conv.Instructions) == 0 && len(raw.Errors) > 0 {
		return nil, fmt.Errorf("twitter API error: %s", raw.Errors[0].Message)
	}
	if len(raw.Errors) > 0 {
		slog.Debug("TweetDetail errors[] alongside data", slog.String("first", raw.Errors[0].Message), slog.Int("count", len(raw.Errors)))
	}
	tl := timelineObj{Instructions: make([]timelineInstruction, 0)}
	pushContent := func(instr *timelineInstruction, entryID string, ic itemContentRef) {
		if ic == nil || isPromotedItem(entryID, ic) {
			return
		}
		instr.Entries = append(instr.Entries, timelineEntry{
			EntryID: entryID,
			Content: timelineContent{ItemContent: ic},
		})
	}
	for _, instr := range conv.Instructions {
		ti := timelineInstruction{Entries: make([]timelineEntry, 0)}
		if instr.Entry != nil {
			pushContent(&ti, instr.Entry.EntryID, instr.Entry.Content.ItemContent)
		}
		for _, e := range instr.Entries {
			pushContent(&ti, e.EntryID, e.Content.ItemContent)
			for _, it := range e.Content.Items {
				pushContent(&ti, it.EntryID, it.Item.ItemContent)
			}
		}
		tl.Instructions = append(tl.Instructions, ti)
	}
	tweets, err := extractTweets(tl, "", false, newOptions(opts))
	if err != nil {
		return nil, err
	}
	if focalID != "" {
		for _, t := range tweets {
			if t.ID == focalID {
				return tweets, nil
			}
		}
		return nil, fmt.Errorf("%w: tweet %s absent from conversation page", model.ErrNotFound, focalID)
	}
	return tweets, nil
}

// isPromotedItem reports whether a timeline item is an ad: promoted entries
// carry promotedMetadata inside itemContent and/or a promoted-* entryId.
// Promoted tweets must not surface as conversation replies.
func isPromotedItem(entryID string, itemContent json.RawMessage) bool {
	if isPromotedEntryID(entryID) {
		return true
	}
	var probe struct {
		Promoted json.RawMessage `json:"promotedMetadata"`
	}
	// A JSON null is "no metadata", not an ad: RawMessage keeps the literal.
	return json.Unmarshal(itemContent, &probe) == nil && hasJSONValue(probe.Promoted)
}

// isPromotedEntryID matches the promoted entry-id shapes: "promoted-tweet-…",
// the camel-case "promotedTweet-…" and a promoted segment inside a longer id.
func isPromotedEntryID(entryID string) bool {
	id := strings.ToLower(entryID)
	return strings.HasPrefix(id, "promoted") || strings.Contains(id, "-promoted")
}

// TweetTimeline parses UserTweets timeline response.
//
// A response whose data carries no timeline object at all (`{"data":{"user":{}}}`,
// a user result without a timeline) is NOT an empty profile: it returns an error
// wrapping model.ErrUnexpectedResponse, so a poller cannot read it as "no new
// tweets". A timeline object that exists with zero entries is a legitimately
// empty profile and succeeds with an empty list. A user X reports as unavailable
// returns model.ErrNotFound.
func TweetTimeline(body []byte, authorID string, opts ...Option) ([]*model.Tweet, error) {
	var raw struct {
		Data struct {
			User struct {
				Result struct {
					TypeName string `json:"__typename"`
					Timeline struct {
						Timeline *timelineObj `json:"timeline"`
					} `json:"timeline"`
					TimelineV2 struct {
						Timeline *timelineObj `json:"timeline"`
					} `json:"timeline_v2"`
				} `json:"result"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal tweet timeline: %w", err)
	}
	res := raw.Data.User.Result
	if res.TypeName == "UserUnavailable" {
		return nil, fmt.Errorf("%w: user unavailable (suspended or restricted)", model.ErrNotFound)
	}
	// Presence of the timeline object, not its content, tells an empty profile
	// from a response that lost its timeline.
	tl := res.Timeline.Timeline
	if tl == nil || (len(tl.Instructions) == 0 && res.TimelineV2.Timeline != nil) {
		tl = res.TimelineV2.Timeline
	}
	if tl == nil {
		return nil, fmt.Errorf("%w: UserTweets response carries no timeline (user result typename=%q)",
			model.ErrUnexpectedResponse, res.TypeName)
	}
	return extractTweetsFromTimeline(*tl, authorID, opts...)
}

// SearchTimeline parses SearchTimeline response.
func SearchTimeline(body []byte, opts ...Option) ([]*model.Tweet, error) {
	var raw struct {
		Data struct {
			SearchByRawQuery struct {
				SearchTimeline struct {
					Timeline timelineObj `json:"timeline"`
				} `json:"search_timeline"`
			} `json:"search_by_raw_query"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal search timeline: %w", err)
	}
	tl := raw.Data.SearchByRawQuery.SearchTimeline.Timeline
	if len(tl.Instructions) == 0 && len(raw.Errors) > 0 {
		return nil, fmt.Errorf("twitter API error: %s", raw.Errors[0].Message)
	}
	if len(raw.Errors) > 0 {
		slog.Debug("search timeline errors[] alongside data", slog.String("first", raw.Errors[0].Message))
	}
	return extractTweetsFromTimeline(tl, "", opts...)
}

// --- T5 read-cluster timeline parsers ---
//
// Every parser below decodes the op's DOCUMENTED typed root path first, then
// falls back to a recursive "find instructions anywhere under data" scan. That
// fallback is the same locate-the-timeline strategy every major public client
// uses (vladkens/twscrape, trevorhobenshield/twitter-api-client, d60/twikit all
// recursively find "instructions"/"entries" rather than hardcoding the wrapper),
// so a rotated or mis-cited wrapper key degrades to "still works" instead of
// "silently empty". The typed path stays primary for clarity + speed.

// timelineTweetParse unmarshals a GraphQL envelope, surfaces API errors, decodes
// the op-specific root via rootFn, and returns the tweets + bottom cursor. When
// rootFn finds no instructions it falls back to the recursive scan and logs a
// warning tagged with op, so a rotated/broken typed root key is observable (the
// happy typed-path hit stays silent — no log noise).
func timelineTweetParse(body []byte, op string, rootFn func(json.RawMessage) timelineObj, opts []Option) ([]*model.Tweet, string, error) {
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, "", fmt.Errorf("unmarshal timeline: %w", err)
	}
	tl := rootFn(env.Data)
	if len(tl.Instructions) == 0 {
		slog.Warn("timeline typed root missed; used recursive fallback", slog.String("op", op))
		tl = findTimelineInstructions(env.Data)
	}
	// errors[] is fatal only when no usable timeline arrived — the same
	// data-wins rule SearchTimeline/UserByScreenName/TweetDetail already
	// follow: X routinely ships partial pages alongside non-fatal errors.
	if len(tl.Instructions) == 0 && len(env.Errors) > 0 {
		return nil, "", fmt.Errorf("twitter API error: %s", env.Errors[0].Message)
	}
	if len(env.Errors) > 0 {
		slog.Debug("timeline errors[] alongside data", slog.String("op", op), slog.String("first", env.Errors[0].Message))
	}
	tweets, err := extractTweetsFromTimeline(tl, "", opts...)
	if err != nil {
		return nil, "", err
	}
	return tweets, bottomCursor(tl), nil
}

// Bookmarks parses the Bookmarks response.
// Root: data.bookmark_timeline_v2.timeline.instructions — CONFIRMED against
// imperatrona/twitter-scraper (bookmark_timeline_v2 struct) + twscrape's
// graphql_timeline_v2_bookmark_timeline feature flag.
func Bookmarks(body []byte, opts ...Option) ([]*model.Tweet, string, error) {
	return timelineTweetParse(body, "Bookmarks", func(data json.RawMessage) timelineObj {
		var r struct {
			BookmarkTimeline struct {
				Timeline timelineObj `json:"timeline"`
			} `json:"bookmark_timeline_v2"`
		}
		_ = json.Unmarshal(data, &r)
		return r.BookmarkTimeline.Timeline
	}, opts)
}

// HomeTimeline parses HomeTimeline / HomeLatestTimeline responses (same shape).
// Root: data.home.home_timeline_urt.instructions — canonical home-timeline shape
// (trevorhobenshield/twitter-api-client home_timeline_urt).
func HomeTimeline(body []byte, opts ...Option) ([]*model.Tweet, string, error) {
	return timelineTweetParse(body, "HomeTimeline", func(data json.RawMessage) timelineObj {
		var r struct {
			Home struct {
				HomeTimelineURT timelineObj `json:"home_timeline_urt"`
			} `json:"home"`
		}
		_ = json.Unmarshal(data, &r)
		return r.Home.HomeTimelineURT
	}, opts)
}

// ListTweets parses the ListLatestTweetsTimeline response.
// Root: data.list.tweets_timeline.timeline.instructions — canonical list shape
// (vladkens/twscrape ListLatestTweetsTimeline).
func ListTweets(body []byte, opts ...Option) ([]*model.Tweet, string, error) {
	return timelineTweetParse(body, "ListLatestTweetsTimeline", func(data json.RawMessage) timelineObj {
		var r struct {
			List struct {
				TweetsTimeline struct {
					Timeline timelineObj `json:"timeline"`
				} `json:"tweets_timeline"`
			} `json:"list"`
		}
		_ = json.Unmarshal(data, &r)
		return r.List.TweetsTimeline.Timeline
	}, opts)
}

// CommunityTweets parses the CommunityTweetsTimeline response.
// Root: data.communityResults.result.ranked_community_timeline.timeline.instructions.
// The data.communityResults.result prefix is CONFIRMED (twscrape models.py uses
// "data.communityResults.result"); the ranked_community_timeline wrapper is
// UNVERIFIED root key — confirm against a live authed response (smoke test). The
// recursive fallback in timelineTweetParse keeps the parser working if the exact
// wrapper key differs at runtime.
func CommunityTweets(body []byte, opts ...Option) ([]*model.Tweet, string, error) {
	return timelineTweetParse(body, "CommunityTweetsTimeline", func(data json.RawMessage) timelineObj {
		var r struct {
			CommunityResults struct {
				Result struct {
					RankedCommunityTimeline struct {
						Timeline timelineObj `json:"timeline"`
					} `json:"ranked_community_timeline"`
				} `json:"result"`
			} `json:"communityResults"`
		}
		_ = json.Unmarshal(data, &r)
		return r.CommunityResults.Result.RankedCommunityTimeline.Timeline
	}, opts)
}

// bottomCursor returns the bottom pagination cursor from a timeline, or "" when
// none is present. Mirrors the cursor logic in extractUsersFromTimeline.
func bottomCursor(tl timelineObj) string {
	for _, instruction := range tl.Instructions {
		entries := instruction.Entries
		if instruction.Entry != nil {
			entries = append(entries, *instruction.Entry)
		}
		for _, entry := range entries {
			if entry.Content.EntryType != "TimelineTimelineCursor" && entry.Content.TypeName != "TimelineTimelineCursor" {
				continue
			}
			if entry.Content.CursorType == "Bottom" || strings.Contains(entry.EntryID, "cursor-bottom") {
				return entry.Content.Value
			}
		}
	}
	return ""
}

// findTimelineInstructions recursively scans an arbitrary decoded GraphQL data
// node for the first object carrying a non-empty "instructions" array and decodes
// that object as a timelineObj. Used as the resilient fallback when a typed root
// path yields nothing (see timelineTweetParse doc).
func findTimelineInstructions(data json.RawMessage) timelineObj {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return timelineObj{}
	}
	node, ok := findInstructionsNode(v)
	if !ok {
		return timelineObj{}
	}
	var tl timelineObj
	if err := json.Unmarshal(node, &tl); err != nil {
		return timelineObj{}
	}
	return tl
}

// findInstructionsNode walks decoded JSON and returns the instructions-bearing
// object holding the MOST total entries, re-marshaled to JSON.
//
// A first-match scan over a map[string]any is non-deterministic: Go randomizes
// map iteration order, so when a response carries ≥2 sibling objects each owning
// an "instructions" array (a primary timeline plus a sidebar/promoted/module
// block), first-match could return any of them — and the wrong, smaller one.
// Instead, collect every candidate and pick the one whose instructions hold the
// most entries (the real timeline always dominates a sidebar/promoted block).
// Maps are traversed in sorted-key order and ties use strict ">", so the first
// candidate in sorted-key order wins a tie — fully deterministic.
func findInstructionsNode(v any) (json.RawMessage, bool) {
	var best json.RawMessage
	bestCount := -1

	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if instr, ok := t["instructions"].([]any); ok && len(instr) > 0 {
				if n := countInstructionEntries(instr); n > bestCount {
					if raw, err := json.Marshal(t); err == nil {
						best = raw
						bestCount = n
					}
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	return best, bestCount >= 0
}

// countInstructionEntries totals the timeline entries across a decoded
// "instructions" array. The real timeline holds far more entries than a
// sidebar/promoted block, so this score drives largest-wins selection in
// findInstructionsNode.
func countInstructionEntries(instructions []any) int {
	total := 0
	for _, instr := range instructions {
		m, ok := instr.(map[string]any)
		if !ok {
			continue
		}
		if entries, ok := m["entries"].([]any); ok {
			total += len(entries)
		}
		if _, ok := m["entry"]; ok {
			total++
		}
	}
	return total
}

// --- Timeline types ---

type timelineObj struct {
	Instructions []timelineInstruction `json:"instructions"`
}

type timelineInstruction struct {
	Type    string          `json:"type"`
	Entries []timelineEntry `json:"entries"`
	// Entry carries the single entry of TimelinePinEntry and
	// TimelineReplaceEntry instructions.
	Entry *timelineEntry `json:"entry"`
	// EntryIDToReplace names the entry a TimelineReplaceEntry supersedes.
	EntryIDToReplace string `json:"entry_id_to_replace"`
	// ModuleEntryID and ModuleItems carry a TimelineAddToModule instruction:
	// items appended to a module delivered earlier.
	ModuleEntryID string               `json:"moduleEntryId"`
	ModuleItems   []timelineModuleItem `json:"moduleItems"`
}

// timelineModuleItem is one item inside a timeline module.
type timelineModuleItem struct {
	EntryID string `json:"entryId"`
	Item    struct {
		ItemContent json.RawMessage `json:"itemContent"`
	} `json:"item"`
}

type timelineEntry struct {
	EntryID   string          `json:"entryId"`
	SortIndex string          `json:"sortIndex"`
	Content   timelineContent `json:"content"`
}

type timelineContent struct {
	EntryType   string          `json:"entryType"`
	TypeName    string          `json:"__typename"`
	ItemContent json.RawMessage `json:"itemContent"`
	// Items holds the members of a TimelineTimelineModule entry (profile
	// conversation threads and similar).
	Items      []timelineModuleItem `json:"items"`
	Value      string               `json:"value"`
	CursorType string               `json:"cursorType"`
}

type userResult struct {
	TypeName string `json:"__typename"`
	ID       string `json:"id"`
	RestID   string `json:"rest_id"`
	// Core holds identity fields that X moved out of `legacy` during the 2026-06
	// GraphQL migration (name / screen_name / created_at). When present it is
	// authoritative; legacy.* is the pre-migration fallback. Verified by live
	// capture against UserByScreenName + UserTweets on 2026-06-23.
	Core struct {
		Name       string `json:"name"`
		ScreenName string `json:"screen_name"`
		CreatedAt  string `json:"created_at"`
	} `json:"core"`
	// Avatar is the post-migration profile-image location
	// (legacy.profile_image_url_https is null for the new shape).
	Avatar struct {
		ImageURL string `json:"image_url"`
	} `json:"avatar"`
	// Verification is the post-migration legacy-verified location
	// (legacy.verified is null for the new shape).
	Verification struct {
		Verified bool `json:"verified"`
	} `json:"verification"`
	// ProfileBio is the post-migration bio location; the live cz_binance
	// capture (2026-06-23) carries it alongside a still-populated
	// legacy.description, the same migration-in-progress signature as core.*.
	ProfileBio struct {
		Description string `json:"description"`
	} `json:"profile_bio"`
	// RelationshipCounts is the post-migration followers/following location.
	// The 2026-10 schema rollout drops `legacy` ENTIRELY for some requests
	// (verified by live capture 2026-10-17: elonmusk via UserByScreenName
	// arrived with core/avatar/profile_bio populated and no legacy key at
	// all — a hard cut, not the hybrid in-progress shape seen for core.*).
	// Pointer so presence (not value) selects the source: a genuine zero
	// count must not fall back to a stale legacy value.
	RelationshipCounts *struct {
		Followers int `json:"followers"`
		Following int `json:"following"`
	} `json:"relationship_counts"`
	// TweetCounts is the post-migration statuses_count location
	// (media_tweets also lives here but has no TwitterUser field).
	TweetCounts *struct {
		Tweets int `json:"tweets"`
	} `json:"tweet_counts"`
	Legacy struct {
		Name            string `json:"name"`
		ScreenName      string `json:"screen_name"`
		FollowersCount  int    `json:"followers_count"`
		FriendsCount    int    `json:"friends_count"`
		StatusesCount   int    `json:"statuses_count"`
		ListedCount     int    `json:"listed_count"`
		CreatedAt       string `json:"created_at"`
		Verified        bool   `json:"verified"`
		Description     string `json:"description"`
		ProfileImageURL string `json:"profile_image_url_https"`
	} `json:"legacy"`
	IsBlueVerified bool `json:"is_blue_verified"`
}

// screenName returns the handle, preferring the post-migration core.screen_name
// and falling back to the pre-migration legacy.screen_name.
func (r userResult) screenName() string {
	if r.Core.ScreenName != "" {
		return r.Core.ScreenName
	}
	return r.Legacy.ScreenName
}

// displayName returns the display name, preferring core.name then legacy.name.
func (r userResult) displayName() string {
	if r.Core.Name != "" {
		return r.Core.Name
	}
	return r.Legacy.Name
}

// createdAtRaw returns the raw created_at string, preferring core then legacy.
func (r userResult) createdAtRaw() string {
	if r.Core.CreatedAt != "" {
		return r.Core.CreatedAt
	}
	return r.Legacy.CreatedAt
}

// avatarURL returns the profile image URL, preferring the post-migration
// avatar.image_url then legacy.profile_image_url_https.
func (r userResult) avatarURL() string {
	if r.Avatar.ImageURL != "" {
		return r.Avatar.ImageURL
	}
	return r.Legacy.ProfileImageURL
}

// bio returns the profile description, preferring the post-migration
// profile_bio.description then the pre-migration legacy.description.
func (r userResult) bio() string {
	if r.ProfileBio.Description != "" {
		return r.ProfileBio.Description
	}
	return r.Legacy.Description
}

// isVerified reports verification from any of three sources: the blue check
// (is_blue_verified), the post-migration verification.verified, or the
// pre-migration legacy.verified.
func (r userResult) isVerified() bool {
	return r.IsBlueVerified || r.Verification.Verified || r.Legacy.Verified
}

// followers returns the follower count, preferring the post-migration
// relationship_counts.followers when that object is present and falling back
// to legacy.followers_count for the still-served old shape.
func (r userResult) followers() int {
	if r.RelationshipCounts != nil {
		return r.RelationshipCounts.Followers
	}
	return r.Legacy.FollowersCount
}

// following returns the friends count from relationship_counts.following when
// present, else legacy.friends_count.
func (r userResult) following() int {
	if r.RelationshipCounts != nil {
		return r.RelationshipCounts.Following
	}
	return r.Legacy.FriendsCount
}

// tweets returns the statuses count from tweet_counts.tweets when present,
// else legacy.statuses_count.
func (r userResult) tweets() int {
	if r.TweetCounts != nil {
		return r.TweetCounts.Tweets
	}
	return r.Legacy.StatusesCount
}

// listed stays legacy-only: no post-migration listed_count location has been
// observed in live captures (the no-legacy shape simply drops the field).
func (r userResult) listed() int {
	return r.Legacy.ListedCount
}

type tweetResult struct {
	TypeName string `json:"__typename"`
	RestID   string `json:"rest_id"`
	Core     struct {
		UserResults struct {
			Result userResult `json:"result"`
		} `json:"user_results"`
	} `json:"core"`
	// Details carries the text and creation time in the x-web response shape,
	// where legacy keeps only conversation metadata (testdata/
	// tweet_notifications.json, xwebTweetResult). A fallback: legacy stays
	// authoritative while it carries the field.
	Details struct {
		FullText    string `json:"full_text"`
		CreatedAtMs int64  `json:"created_at_ms"`
	} `json:"details"`
	Legacy struct {
		FullText      string `json:"full_text"`
		CreatedAt     string `json:"created_at"`
		FavoriteCount int    `json:"favorite_count"`
		RetweetCount  int    `json:"retweet_count"`
		QuoteCount    int    `json:"quote_count"`
		ReplyCount    int    `json:"reply_count"`
		UserIDStr     string `json:"user_id_str"`
		// Entities and ExtendedEntities carry the attached media; a media-only
		// tweet legitimately has empty text.
		Entities         mediaEntities `json:"entities"`
		ExtendedEntities mediaEntities `json:"extended_entities"`
		// InReplyToStatusID, QuotedStatusID: relationship markers.
		InReplyToStatusID string `json:"in_reply_to_status_id_str"`
		QuotedStatusID    string `json:"quoted_status_id_str"`
		// RetweetedStatus is the original of a retweet; full_text of the
		// retweet itself is the "RT @user: …" form X cuts short.
		RetweetedStatus struct {
			Result *tweetResult `json:"result"`
		} `json:"retweeted_status_result"`
	} `json:"legacy"`
	// Card is the link/poll card of a tweet; a tweet may carry only that.
	Card json.RawMessage `json:"card"`
	// NoteTweet holds the untruncated text of a long tweet; legacy.full_text
	// is cut at ~280 characters for those.
	NoteTweet struct {
		Results struct {
			Result struct {
				Text string `json:"text"`
			} `json:"result"`
		} `json:"note_tweet_results"`
	} `json:"note_tweet"`
	QuotedStatus struct {
		Result *tweetResult `json:"result"`
	} `json:"quoted_status_result"`
	// EditControl: a later edit carries initial_tweet_id (and the id list
	// under edit_control_initial); a first version carries the id list flat.
	EditControl struct {
		InitialTweetID     string   `json:"initial_tweet_id"`
		EditTweetIDs       []string `json:"edit_tweet_ids"`
		EditControlInitial struct {
			EditTweetIDs []string `json:"edit_tweet_ids"`
		} `json:"edit_control_initial"`
	} `json:"edit_control"`
	Views struct {
		Count flexCount `json:"count"`
	} `json:"views"`
	// TweetWithVisibilityResults nests the real tweet under "tweet";
	// any future *Results wrapper would land in the same field.
	Tweet *tweetResult `json:"tweet"`
}

// --- Extraction helpers ---

func extractUsersFromTimeline(tl timelineObj) ([]*model.TwitterUser, string, error) {
	pg := extractUserPage(tl)
	return pg.Users, pg.Cursor, nil
}

func extractUserPage(tl timelineObj) UserPage {
	var users []*model.TwitterUser
	var nextCursor string
	var entriesSeen int

	for _, instruction := range tl.Instructions {
		entries := instruction.Entries
		if instruction.Entry != nil {
			entries = append(entries, *instruction.Entry)
		}
		for _, entry := range entries {
			if entry.Content.EntryType == "TimelineTimelineCursor" || entry.Content.TypeName == "TimelineTimelineCursor" {
				if entry.Content.CursorType == "Bottom" || strings.Contains(entry.EntryID, "cursor-bottom") {
					nextCursor = entry.Content.Value
				}
				continue
			}
			if entry.Content.ItemContent == nil {
				continue
			}
			var item struct {
				TypeName    string `json:"__typename"`
				UserResults struct {
					Result userResult `json:"result"`
				} `json:"user_results"`
			}
			if err := json.Unmarshal(entry.Content.ItemContent, &item); err != nil {
				continue
			}
			if item.TypeName != "TimelineUser" {
				continue
			}
			entriesSeen++
			u, err := parseUserResult(item.UserResults.Result)
			if err != nil {
				slog.Debug("skip user parse error", slog.Any("error", err))
				continue
			}
			users = append(users, u)
		}
	}
	return UserPage{Users: users, Cursor: nextCursor, Entries: entriesSeen}
}

// timelineItem is one tweet-bearing item located inside a timeline: a plain
// entry, a member of a module, or the single entry of a pin/replace
// instruction.
type timelineItem struct {
	entryID     string
	itemContent json.RawMessage
	pinned      bool
	// moduleID is the entry id of the module holding the item, "" for a plain
	// entry; parentPromoted is set when that module is promoted.
	moduleID       string
	parentPromoted bool
}

// items lists every item of an instruction in delivery order: the single
// entry first (a pinned tweet leads its page), then the entries and their
// module members, then TimelineAddToModule members.
func (ins timelineInstruction) items() []timelineItem {
	var out []timelineItem
	add := func(e timelineEntry, pinned bool) {
		if e.Content.ItemContent != nil {
			out = append(out, timelineItem{entryID: e.EntryID, itemContent: e.Content.ItemContent, pinned: pinned})
		}
		promoted := isPromotedEntryID(e.EntryID)
		for _, mi := range e.Content.Items {
			out = append(out, timelineItem{entryID: mi.EntryID, itemContent: mi.Item.ItemContent, moduleID: e.EntryID, parentPromoted: promoted})
		}
	}
	if ins.Entry != nil {
		add(*ins.Entry, ins.Type == "TimelinePinEntry")
	}
	for _, e := range ins.Entries {
		add(e, false)
	}
	promoted := isPromotedEntryID(ins.ModuleEntryID)
	for _, mi := range ins.ModuleItems {
		out = append(out, timelineItem{entryID: mi.EntryID, itemContent: mi.Item.ItemContent, moduleID: ins.ModuleEntryID, parentPromoted: promoted})
	}
	return out
}

func extractTweetsFromTimeline(tl timelineObj, defaultAuthorID string, opts ...Option) ([]*model.Tweet, error) {
	return extractTweets(tl, defaultAuthorID, true, newOptions(opts))
}

// extractTweets walks the timeline. dedupe collapses repeated ids; TweetDetail
// turns it off because the focal tweet legitimately reappears as the anchor of
// its own conversation module (see SplitConversation).
func extractTweets(tl timelineObj, defaultAuthorID string, dedupe bool, o options) ([]*model.Tweet, error) {
	var tweets []*model.Tweet
	// entryId (and module entry id) -> indexes in tweets, for TimelineReplaceEntry.
	byEntry := map[string][]int{}

	for _, instruction := range tl.Instructions {
		// A replace instruction supersedes the tweet delivered under the
		// named entry (a module's items included): drop them so the page does
		// not carry both versions.
		if instruction.Type == "TimelineReplaceEntry" && instruction.EntryIDToReplace != "" {
			for _, idx := range byEntry[instruction.EntryIDToReplace] {
				tweets[idx] = nil
			}
			delete(byEntry, instruction.EntryIDToReplace)
		}
		for _, it := range instruction.items() {
			if it.itemContent == nil {
				continue
			}
			var item struct {
				TypeName     string `json:"__typename"`
				TweetResults struct {
					Result tweetResult `json:"result"`
				} `json:"tweet_results"`
			}
			if err := json.Unmarshal(it.itemContent, &item); err != nil {
				slog.Debug("skip timeline item that does not decode", slog.String("entry", it.entryID), slog.Any("error", err))
				continue
			}
			if item.TypeName != "TimelineTweet" {
				switch item.TypeName {
				case "TimelineUser", "TimelinePrompt", "TimelineMessagePrompt", "TimelineTimelineCursor":
				default:
					o.drop(DropUnhandledType)
					slog.Debug("skip timeline item of unhandled type", slog.String("entry", it.entryID), slog.String("type", item.TypeName))
				}
				continue
			}
			t, err := parseTweetResult(item.TweetResults.Result, defaultAuthorID, o)
			if err != nil {
				o.drop(dropReason(err))
				slog.Debug("skip tweet parse error", slog.String("entry", it.entryID), slog.Any("error", err))
				continue
			}
			t.Promoted = it.parentPromoted || isPromotedItem(it.entryID, it.itemContent)
			t.Pinned = it.pinned
			byEntry[it.entryID] = append(byEntry[it.entryID], len(tweets))
			if it.moduleID != "" {
				byEntry[it.moduleID] = append(byEntry[it.moduleID], len(tweets))
			}
			tweets = append(tweets, t)
		}
	}
	// Drop superseded tweets and later copies of an id already seen (a pinned
	// tweet is usually delivered again among the entries). The first copy
	// stays, and keeps Pinned if any copy was pinned.
	var kept []*model.Tweet
	seen := map[string]*model.Tweet{}
	for _, t := range tweets {
		if t == nil {
			continue
		}
		if first, dup := seen[t.ID]; dedupe && dup {
			first.Pinned = first.Pinned || t.Pinned
			continue
		}
		seen[t.ID] = t
		kept = append(kept, t)
	}
	return kept, nil
}

// parseTwitterTime parses X's created_at format. An empty input yields the zero
// time silently (absent field is normal). A non-empty value that fails to parse
// also yields the zero time but logs a breadcrumb: this is the silent-zero
// failure class — if X migrates the created_at format the way it migrated the
// identity fields out of legacy.*, the field would otherwise zero with no
// signal. The site label identifies which path surfaced the unparseable value.
func parseTwitterTime(raw, site string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse("Mon Jan 02 15:04:05 +0000 2006", raw)
	if err != nil {
		slog.Debug("created_at non-empty but unparseable; field zeroed",
			slog.String("site", site),
			slog.String("raw", raw),
			slog.Any("error", err))
		return time.Time{}
	}
	return t
}

func parseUserResult(r userResult) (*model.TwitterUser, error) {
	if r.TypeName == "UserUnavailable" {
		return nil, fmt.Errorf("%w: user unavailable (suspended or restricted)", model.ErrNotFound)
	}
	if r.RestID == "" {
		return nil, fmt.Errorf("%w: empty user rest_id (typename=%s)", model.ErrNotFound, r.TypeName)
	}
	createdAt := parseTwitterTime(r.createdAtRaw(), "user")
	bio := strings.TrimSpace(r.bio())
	avatarURL := r.avatarURL()
	return &model.TwitterUser{
		ID:          r.RestID,
		Handle:      r.screenName(),
		DisplayName: r.displayName(),
		Bio:         bio,
		Followers:   r.followers(),
		Following:   r.following(),
		TweetCount:  r.tweets(),
		ListedCount: r.listed(),
		CreatedAt:   createdAt,
		IsVerified:  r.isVerified(),
		HasAvatar:   avatarURL != "" && !strings.Contains(avatarURL, "default_profile"),
		HasBio:      bio != "",
	}, nil
}

func parseTweetResult(r tweetResult, defaultAuthorID string, o options) (*model.Tweet, error) {
	// Unwrap *Results shells (TweetWithVisibilityResults for sensitive
	// tweets): the payload nests the real tweet one level down. Loop —
	// not a fixed depth — so a stacked wrapper still decodes.
	for r.RestID == "" && r.Tweet != nil {
		r = *r.Tweet
	}
	if r.RestID == "" {
		reason := DropNoID
		if r.TypeName == "TweetTombstone" || r.TypeName == "TweetUnavailable" {
			reason = DropTombstone
		}
		return nil, &dropError{reason: reason, err: fmt.Errorf("empty tweet rest_id (typename=%s)", r.TypeName)}
	}

	// The tweet's own author wins over the profile owner the caller passed in:
	// a module can carry other authors' tweets.
	authorID := defaultAuthorID
	if id := r.Core.UserResults.Result.RestID; id != "" {
		authorID = id
	} else if r.Legacy.UserIDStr != "" {
		authorID = r.Legacy.UserIDStr
	}

	// Creation time: legacy.created_at, then details.created_at_ms, then the
	// time encoded in the snowflake id. A tweet none of them dates is dropped
	// (counted) rather than returned with a zero time.
	createdAt := parseTwitterTime(r.Legacy.CreatedAt, "tweet")
	if createdAt.IsZero() && r.Details.CreatedAtMs > 0 {
		createdAt = time.UnixMilli(r.Details.CreatedAtMs).UTC()
	}
	if createdAt.IsZero() {
		if ts, err := SnowflakeTime(r.RestID); err == nil {
			createdAt = ts
			o.createdAtFallback()
		}
	}
	if createdAt.IsZero() {
		return nil, &dropError{reason: DropNoCreatedAt, err: fmt.Errorf("tweet %s has no creation time", r.RestID)}
	}

	views := 0
	if r.Views.Count != "" {
		var err error
		if views, err = strconv.Atoi(string(r.Views.Count)); err != nil {
			slog.Debug("views.count is not an integer; field zeroed", slog.String("raw", string(r.Views.Count)))
		}
	}

	// legacy.full_text is cut for long tweets; note_tweet holds the whole text.
	text := r.Legacy.FullText
	if text == "" {
		text = r.Details.FullText
	}
	if nt := r.NoteTweet.Results.Result.Text; nt != "" {
		text = nt
	}

	author := r.Core.UserResults.Result
	t := &model.Tweet{
		ID:           r.RestID,
		AuthorID:     authorID,
		AuthorHandle: author.screenName(),
		AuthorName:   author.displayName(),
		Text:         text,
		CreatedAt:    createdAt,
		Views:        views,
		Likes:        r.Legacy.FavoriteCount,
		Retweets:     r.Legacy.RetweetCount,
		Quotes:       r.Legacy.QuoteCount,
		ReplyCount:   r.Legacy.ReplyCount,
		InReplyToID:  r.Legacy.InReplyToStatusID,
		QuotedID:     r.Legacy.QuotedStatusID,
		EditOf:       r.editOf(),
	}
	if t.QuotedID == "" && r.QuotedStatus.Result != nil {
		t.QuotedID = innerRestID(r.QuotedStatus.Result)
	}
	if rs := r.Legacy.RetweetedStatus.Result; rs != nil {
		t.RetweetOfID = innerRestID(rs)
		if orig, err := parseTweetResult(*rs, "", options{}); err == nil {
			t.RetweetOf = orig
			t.RetweetOfID = orig.ID
			// The retweet's own full_text is the "RT @user: …" form X cuts
			// short; rebuild it from the untruncated original.
			if orig.AuthorHandle != "" {
				t.Text = "RT @" + orig.AuthorHandle + ": " + orig.Text
			}
		} else {
			slog.Debug("retweet original not decodable", slog.String("retweet", t.ID), slog.Any("error", err))
		}
	}
	// Empty text with nothing to explain it (media, card, quote, retweet) is
	// likely a blanked parse: count it, but keep the tweet.
	if t.Text == "" && !r.hasAttachment() && t.QuotedID == "" && t.RetweetOfID == "" {
		o.drop(DropBlankText)
	}
	t.TokenMentions = extractTokenMentions(t.Text)
	return t, nil
}

// mediaEntities is the media list of an entities block.
type mediaEntities struct {
	Media []json.RawMessage `json:"media"`
}

// hasAttachment reports media or a card on the tweet.
func (r tweetResult) hasAttachment() bool {
	return len(r.Legacy.Entities.Media) > 0 || len(r.Legacy.ExtendedEntities.Media) > 0 || hasJSONValue(r.Card)
}

// innerRestID returns the rest_id of a result after unwrapping *Results
// shells, "" for a tombstone or unavailable result.
func innerRestID(r *tweetResult) string {
	for r != nil && r.RestID == "" {
		r = r.Tweet
	}
	if r == nil {
		return ""
	}
	return r.RestID
}

// editOf returns the first version's id when this tweet is a later edit of it.
func (r tweetResult) editOf() string {
	initial := r.EditControl.InitialTweetID
	if initial == "" && len(r.EditControl.EditControlInitial.EditTweetIDs) > 0 {
		initial = r.EditControl.EditControlInitial.EditTweetIDs[0]
	}
	if initial == "" && len(r.EditControl.EditTweetIDs) > 0 {
		initial = r.EditControl.EditTweetIDs[0]
	}
	if initial == r.RestID {
		return ""
	}
	return initial
}

// flexCount is a counter X ships as a numeric string ("147516") but that must
// not fail the whole tweet when it arrives as a bare number or is absent.
type flexCount string

func (c *flexCount) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case strings.HasPrefix(s, `"`):
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return nil
		}
		*c = flexCount(v)
	case s == "null" || s == "":
	default:
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			*c = flexCount(s)
		}
	}
	return nil
}

// CreateTweet extracts the tweet ID from a CreateTweet mutation response.
func CreateTweet(body []byte) (string, error) {
	var raw struct {
		Data struct {
			CreateTweet struct {
				TweetResults struct {
					Result struct {
						RestID string `json:"rest_id"`
					} `json:"result"`
				} `json:"tweet_results"`
			} `json:"create_tweet"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("unmarshal CreateTweet: %w", err)
	}
	if len(raw.Errors) > 0 {
		return "", fmt.Errorf("CreateTweet API error: %s", raw.Errors[0].Message)
	}
	tweetID := raw.Data.CreateTweet.TweetResults.Result.RestID
	if tweetID == "" {
		return "", fmt.Errorf("CreateTweet returned empty tweet ID: %s", TruncateBytes(body, 300))
	}
	return tweetID, nil
}

// Ack validates a bare "Done" acknowledgement from a like/unlike mutation
// response. op is the op name for error context; key is the data field carrying
// the ack ("favorite_tweet" / "unfavorite_tweet"). Mirrors CreateTweet's
// handling: surface API errors[] first, then on a missing or non-"Done" ack
// return an error with a truncated body (never a silent success).
func Ack(body []byte, op, key string) error {
	var raw struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("unmarshal %s: %w", op, err)
	}
	if len(raw.Errors) > 0 {
		return fmt.Errorf("%s API error: %s", op, raw.Errors[0].Message)
	}
	var ack string
	if v, ok := raw.Data[key]; ok {
		_ = json.Unmarshal(v, &ack)
	}
	if ack != "Done" {
		return fmt.Errorf("%s did not return \"Done\": %s", op, TruncateBytes(body, 300))
	}
	return nil
}

// CreateRetweet extracts the new retweet's rest_id from a CreateRetweet
// mutation response. Mirrors CreateTweet's error+empty handling.
func CreateRetweet(body []byte) (string, error) {
	var raw struct {
		Data struct {
			CreateRetweet struct {
				RetweetResults struct {
					Result struct {
						RestID string `json:"rest_id"`
					} `json:"result"`
				} `json:"retweet_results"`
			} `json:"create_retweet"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("unmarshal CreateRetweet: %w", err)
	}
	if len(raw.Errors) > 0 {
		return "", fmt.Errorf("CreateRetweet API error: %s", raw.Errors[0].Message)
	}
	id := raw.Data.CreateRetweet.RetweetResults.Result.RestID
	if id == "" {
		return "", fmt.Errorf("CreateRetweet returned empty retweet ID: %s", TruncateBytes(body, 300))
	}
	return id, nil
}

// DeleteRetweet extracts the source tweet's rest_id from a DeleteRetweet
// mutation response (the op key is "unretweet"). Mirrors CreateTweet's
// error+empty handling.
func DeleteRetweet(body []byte) (string, error) {
	var raw struct {
		Data struct {
			Unretweet struct {
				SourceTweetResults struct {
					Result struct {
						RestID string `json:"rest_id"`
					} `json:"result"`
				} `json:"source_tweet_results"`
			} `json:"unretweet"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("unmarshal DeleteRetweet: %w", err)
	}
	if len(raw.Errors) > 0 {
		return "", fmt.Errorf("DeleteRetweet API error: %s", raw.Errors[0].Message)
	}
	id := raw.Data.Unretweet.SourceTweetResults.Result.RestID
	if id == "" {
		return "", fmt.Errorf("DeleteRetweet returned empty source tweet ID: %s", TruncateBytes(body, 300))
	}
	return id, nil
}

func extractTokenMentions(text string) []string {
	matches := tokenMentionRe.FindAllStringSubmatch(strings.ToUpper(text), -1)
	seen := make(map[string]bool)
	var result []string
	for _, m := range matches {
		if len(m) >= 2 && !seen[m[1]] {
			seen[m[1]] = true
			result = append(result, m[1])
		}
	}
	return result
}

// SearchUsersTimeline parses a People-tab SearchTimeline response —
// same envelope as SearchTimeline, user entries instead of tweets.
func SearchUsersTimeline(body []byte) ([]*model.TwitterUser, error) {
	var raw struct {
		Data struct {
			SearchByRawQuery struct {
				SearchTimeline struct {
					Timeline timelineObj `json:"timeline"`
				} `json:"search_timeline"`
			} `json:"search_by_raw_query"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal search users timeline: %w", err)
	}
	tl := raw.Data.SearchByRawQuery.SearchTimeline.Timeline
	if len(tl.Instructions) == 0 && len(raw.Errors) > 0 {
		return nil, fmt.Errorf("twitter API error: %s", raw.Errors[0].Message)
	}
	if len(raw.Errors) > 0 {
		slog.Debug("search users errors[] alongside data", slog.String("first", raw.Errors[0].Message))
	}
	users, _, err := extractUsersFromTimeline(tl)
	return users, err
}

// SplitConversation picks the focal tweet (ID match, else first entry) out of
// a TweetDetail conversation page and returns the rest as replies in entry
// order. Every copy of the focal ID is excluded from replies — the focal
// reappears inside its own conversationthread-* module as the anchor item.
func SplitConversation(tweets []*model.Tweet, focalID string) (focal *model.Tweet, replies []*model.Tweet) {
	if len(tweets) == 0 {
		return nil, nil
	}
	focalIdx := 0
	for i, t := range tweets {
		if t.ID == focalID {
			focalIdx = i
			break
		}
	}
	focal = tweets[focalIdx]
	for i, t := range tweets {
		if i != focalIdx && t.ID != focal.ID {
			replies = append(replies, t)
		}
	}
	return focal, replies
}
