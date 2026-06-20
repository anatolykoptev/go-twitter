package twitter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var tokenMentionRe = regexp.MustCompile(`\$([A-Z]{2,10})`)

// parseUserByScreenName parses the UserByScreenName GraphQL response.
func parseUserByScreenName(body []byte) (*TwitterUser, error) {
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
	if len(raw.Errors) > 0 {
		return nil, fmt.Errorf("twitter API error: %s", raw.Errors[0].Message)
	}
	return parseUserResult(raw.Data.User.Result)
}

// parseUserList parses Followers/Following response.
func parseUserList(body []byte) ([]*TwitterUser, string, error) {
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
		return nil, "", fmt.Errorf("unmarshal user list: %w", err)
	}
	return extractUsersFromTimeline(raw.Data.User.Result.Timeline.Timeline)
}

// parseRetweeterList parses Retweeters response.
func parseRetweeterList(body []byte) ([]*TwitterUser, string, error) {
	var raw struct {
		Data struct {
			RetweetersTimeline struct {
				Timeline timelineObj `json:"timeline"`
			} `json:"retweeters_timeline"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", fmt.Errorf("unmarshal retweeter list: %w", err)
	}
	tl := raw.Data.RetweetersTimeline.Timeline
	if len(tl.Instructions) == 0 {
		return parseUserList(body)
	}
	return extractUsersFromTimeline(tl)
}

// parseTweetDetail parses TweetDetail GraphQL response.
// The response wraps tweets in a threaded conversation timeline.
func parseTweetDetail(body []byte) ([]*Tweet, error) {
	type conversationData struct {
		Instructions []struct {
			Entries []struct {
				Content struct {
					ItemContent json.RawMessage `json:"itemContent"`
				} `json:"content"`
			} `json:"entries"`
		} `json:"instructions"`
	}
	var raw struct {
		Data struct {
			// Twitter uses both keys depending on the endpoint version
			V2 conversationData `json:"threaded_conversation_with_injections_v2"`
			V1 conversationData `json:"threaded_conversation_with_injections"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal TweetDetail: %w", err)
	}
	// Use v2 if it has instructions, otherwise fall back to v1
	conv := raw.Data.V2
	if len(conv.Instructions) == 0 {
		conv = raw.Data.V1
	}
	tl := timelineObj{Instructions: make([]timelineInstruction, 0)}
	for _, instr := range conv.Instructions {
		entries := make([]timelineEntry, 0, len(instr.Entries))
		for _, e := range instr.Entries {
			entries = append(entries, timelineEntry{
				Content: timelineContent{ItemContent: e.Content.ItemContent},
			})
		}
		tl.Instructions = append(tl.Instructions, timelineInstruction{Entries: entries})
	}
	return extractTweetsFromTimeline(tl, "")
}

// parseTweetTimeline parses UserTweets timeline response.
func parseTweetTimeline(body []byte, authorID string) ([]*Tweet, error) {
	var raw struct {
		Data struct {
			User struct {
				Result struct {
					Timeline struct {
						Timeline timelineObj `json:"timeline"`
					} `json:"timeline"`
					TimelineV2 struct {
						Timeline timelineObj `json:"timeline"`
					} `json:"timeline_v2"`
				} `json:"result"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal tweet timeline: %w", err)
	}
	tl := raw.Data.User.Result.Timeline.Timeline
	if len(tl.Instructions) == 0 {
		tl = raw.Data.User.Result.TimelineV2.Timeline
	}
	return extractTweetsFromTimeline(tl, authorID)
}

// parseSearchTimeline parses SearchTimeline response.
func parseSearchTimeline(body []byte) ([]*Tweet, error) {
	var raw struct {
		Data struct {
			SearchByRawQuery struct {
				SearchTimeline struct {
					Timeline timelineObj `json:"timeline"`
				} `json:"search_timeline"`
			} `json:"search_by_raw_query"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal search timeline: %w", err)
	}
	return extractTweetsFromTimeline(raw.Data.SearchByRawQuery.SearchTimeline.Timeline, "")
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
func timelineTweetParse(body []byte, op string, rootFn func(json.RawMessage) timelineObj) ([]*Tweet, string, error) {
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, "", fmt.Errorf("unmarshal timeline: %w", err)
	}
	if len(env.Errors) > 0 {
		return nil, "", fmt.Errorf("twitter API error: %s", env.Errors[0].Message)
	}
	tl := rootFn(env.Data)
	if len(tl.Instructions) == 0 {
		slog.Warn("timeline typed root missed; used recursive fallback", slog.String("op", op))
		tl = findTimelineInstructions(env.Data)
	}
	tweets, err := extractTweetsFromTimeline(tl, "")
	if err != nil {
		return nil, "", err
	}
	return tweets, bottomCursor(tl), nil
}

// parseBookmarks parses the Bookmarks response.
// Root: data.bookmark_timeline_v2.timeline.instructions — CONFIRMED against
// imperatrona/twitter-scraper (bookmark_timeline_v2 struct) + twscrape's
// graphql_timeline_v2_bookmark_timeline feature flag.
func parseBookmarks(body []byte) ([]*Tweet, string, error) {
	return timelineTweetParse(body, "Bookmarks", func(data json.RawMessage) timelineObj {
		var r struct {
			BookmarkTimeline struct {
				Timeline timelineObj `json:"timeline"`
			} `json:"bookmark_timeline_v2"`
		}
		_ = json.Unmarshal(data, &r)
		return r.BookmarkTimeline.Timeline
	})
}

// parseHomeTimeline parses HomeTimeline / HomeLatestTimeline responses (same shape).
// Root: data.home.home_timeline_urt.instructions — canonical home-timeline shape
// (trevorhobenshield/twitter-api-client home_timeline_urt).
func parseHomeTimeline(body []byte) ([]*Tweet, string, error) {
	return timelineTweetParse(body, "HomeTimeline", func(data json.RawMessage) timelineObj {
		var r struct {
			Home struct {
				HomeTimelineURT timelineObj `json:"home_timeline_urt"`
			} `json:"home"`
		}
		_ = json.Unmarshal(data, &r)
		return r.Home.HomeTimelineURT
	})
}

// parseListTweets parses the ListLatestTweetsTimeline response.
// Root: data.list.tweets_timeline.timeline.instructions — canonical list shape
// (vladkens/twscrape ListLatestTweetsTimeline).
func parseListTweets(body []byte) ([]*Tweet, string, error) {
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
	})
}

// parseCommunityTweets parses the CommunityTweetsTimeline response.
// Root: data.communityResults.result.ranked_community_timeline.timeline.instructions.
// The data.communityResults.result prefix is CONFIRMED (twscrape models.py uses
// "data.communityResults.result"); the ranked_community_timeline wrapper is
// UNVERIFIED root key — confirm against a live authed response (smoke test). The
// recursive fallback in timelineTweetParse keeps the parser working if the exact
// wrapper key differs at runtime.
func parseCommunityTweets(body []byte) ([]*Tweet, string, error) {
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
	})
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
	Entry   *timelineEntry  `json:"entry"`
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
	Value       string          `json:"value"`
	CursorType  string          `json:"cursorType"`
}

type userResult struct {
	TypeName string `json:"__typename"`
	ID       string `json:"id"`
	RestID   string `json:"rest_id"`
	Legacy   struct {
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

type tweetResult struct {
	TypeName string `json:"__typename"`
	RestID   string `json:"rest_id"`
	Core     struct {
		UserResults struct {
			Result userResult `json:"result"`
		} `json:"user_results"`
	} `json:"core"`
	Legacy struct {
		FullText      string `json:"full_text"`
		CreatedAt     string `json:"created_at"`
		FavoriteCount int    `json:"favorite_count"`
		RetweetCount  int    `json:"retweet_count"`
		QuoteCount    int    `json:"quote_count"`
		ReplyCount    int    `json:"reply_count"`
		UserIDStr     string `json:"user_id_str"`
	} `json:"legacy"`
	Views struct {
		Count string `json:"count"`
	} `json:"views"`
}

// --- Extraction helpers ---

func extractUsersFromTimeline(tl timelineObj) ([]*TwitterUser, string, error) {
	var users []*TwitterUser
	var nextCursor string

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
			u, err := parseUserResult(item.UserResults.Result)
			if err != nil {
				slog.Debug("skip user parse error", slog.Any("error", err))
				continue
			}
			users = append(users, u)
		}
	}
	return users, nextCursor, nil
}

func extractTweetsFromTimeline(tl timelineObj, defaultAuthorID string) ([]*Tweet, error) {
	var tweets []*Tweet

	for _, instruction := range tl.Instructions {
		for _, entry := range instruction.Entries {
			if entry.Content.ItemContent == nil {
				continue
			}
			var item struct {
				TypeName     string `json:"__typename"`
				TweetResults struct {
					Result tweetResult `json:"result"`
				} `json:"tweet_results"`
			}
			if err := json.Unmarshal(entry.Content.ItemContent, &item); err != nil {
				continue
			}
			if item.TypeName != "TimelineTweet" {
				continue
			}
			t, err := parseTweetResult(item.TweetResults.Result, defaultAuthorID)
			if err != nil {
				slog.Debug("skip tweet parse error", slog.Any("error", err))
				continue
			}
			tweets = append(tweets, t)
		}
	}
	return tweets, nil
}

func parseUserResult(r userResult) (*TwitterUser, error) {
	if r.TypeName == "UserUnavailable" {
		return nil, fmt.Errorf("user unavailable (suspended or restricted)")
	}
	if r.RestID == "" {
		return nil, fmt.Errorf("empty user rest_id (typename=%s)", r.TypeName)
	}
	var createdAt time.Time
	if r.Legacy.CreatedAt != "" {
		t, err := time.Parse("Mon Jan 02 15:04:05 +0000 2006", r.Legacy.CreatedAt)
		if err == nil {
			createdAt = t
		}
	}
	bio := strings.TrimSpace(r.Legacy.Description)
	return &TwitterUser{
		ID:          r.RestID,
		Handle:      r.Legacy.ScreenName,
		DisplayName: r.Legacy.Name,
		Bio:         bio,
		Followers:   r.Legacy.FollowersCount,
		Following:   r.Legacy.FriendsCount,
		TweetCount:  r.Legacy.StatusesCount,
		ListedCount: r.Legacy.ListedCount,
		CreatedAt:   createdAt,
		IsVerified:  r.Legacy.Verified || r.IsBlueVerified,
		HasAvatar:   r.Legacy.ProfileImageURL != "" && !strings.Contains(r.Legacy.ProfileImageURL, "default_profile"),
		HasBio:      bio != "",
	}, nil
}

func parseTweetResult(r tweetResult, defaultAuthorID string) (*Tweet, error) {
	if r.RestID == "" {
		return nil, fmt.Errorf("empty tweet rest_id")
	}

	authorID := defaultAuthorID
	if r.Legacy.UserIDStr != "" {
		authorID = r.Legacy.UserIDStr
	}

	var createdAt time.Time
	if r.Legacy.CreatedAt != "" {
		t, err := time.Parse("Mon Jan 02 15:04:05 +0000 2006", r.Legacy.CreatedAt)
		if err == nil {
			createdAt = t
		}
	}

	views := 0
	if r.Views.Count != "" {
		views, _ = strconv.Atoi(r.Views.Count)
	}

	text := r.Legacy.FullText
	mentions := extractTokenMentions(text)

	return &Tweet{
		ID:            r.RestID,
		AuthorID:      authorID,
		AuthorHandle:  r.Core.UserResults.Result.Legacy.ScreenName,
		AuthorName:    r.Core.UserResults.Result.Legacy.Name,
		Text:          text,
		CreatedAt:     createdAt,
		Views:         views,
		Likes:         r.Legacy.FavoriteCount,
		Retweets:      r.Legacy.RetweetCount,
		Quotes:        r.Legacy.QuoteCount,
		ReplyCount:    r.Legacy.ReplyCount,
		TokenMentions: mentions,
	}, nil
}

// parseCreateTweet extracts the tweet ID from a CreateTweet mutation response.
func parseCreateTweet(body []byte) (string, error) {
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
		return "", fmt.Errorf("CreateTweet returned empty tweet ID: %s", truncateBytes(body, 300))
	}
	return tweetID, nil
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
