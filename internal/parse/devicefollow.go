package parse

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anatolykoptev/go-twitter/model"
)

// TweetNotificationsOp is the GraphQL operation behind the web client's
// notification-bell timeline. The root package aliases it as its endpoint key.
const TweetNotificationsOp = "tweetNotificationsQuery"

// DeviceFollow decodes the globalObjects shape: device_follow.json
// answers like the 1.5 API, with all tweets under globalObjects.tweets and
// their authors under globalObjects.users keyed by user id.
//
// globalObjects.tweets also carries tweets the feed merely references (the
// original behind a retweet, whose author the viewer may not follow), so when the
// timeline lists tweet entries only those are returned. Tweets come back newest
// first (descending snowflake id) with CreatedAt set.
func DeviceFollow(body []byte, opts ...Option) ([]*model.Tweet, error) {
	o := newOptions(opts)
	var raw struct {
		GlobalObjects struct {
			Tweets map[string]struct {
				IDStr     string `json:"id_str"`
				FullText  string `json:"full_text"`
				Text      string `json:"text"`
				UserIDStr string `json:"user_id_str"`
				CreatedAt string `json:"created_at"`
			} `json:"tweets"`
			Users map[string]struct {
				ScreenName string `json:"screen_name"`
				Name       string `json:"name"`
			} `json:"users"`
		} `json:"globalObjects"`
		Timeline json.RawMessage `json:"timeline"`
		Errors   []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal device_follow: %w", err)
	}
	if len(raw.Errors) > 0 {
		return nil, fmt.Errorf("twitter API error %d: %s", raw.Errors[0].Code, raw.Errors[0].Message)
	}
	// A present timeline is authoritative, even when it lists nothing (for
	// example only promoted entries): the tweets outside it are references, not
	// feed items. Without any timeline there is nothing to filter by.
	hasTimeline := len(raw.Timeline) > 0 && string(raw.Timeline) != "null"
	listed := map[string]struct{}{}
	if hasTimeline {
		var tl any
		if err := json.Unmarshal(raw.Timeline, &tl); err != nil {
			return nil, fmt.Errorf("unmarshal device_follow timeline: %w", err)
		}
		collectListedTweets(tl, listed)
	} else {
		noteDeviceFollowWithoutTimeline()
	}
	tweets := make([]*model.Tweet, 0, len(raw.GlobalObjects.Tweets))
	for key, lt := range raw.GlobalObjects.Tweets {
		if lt.IDStr == "" {
			o.drop(DropNoID)
			continue
		}
		if hasTimeline {
			if _, ok := listed[lt.IDStr]; !ok {
				if _, ok := listed[key]; !ok {
					continue
				}
			}
		}
		text := lt.FullText
		if text == "" {
			text = lt.Text
		}
		t := &model.Tweet{ID: lt.IDStr, AuthorID: lt.UserIDStr, Text: text}
		t.CreatedAt = parseTwitterTime(lt.CreatedAt, "device_follow")
		if t.CreatedAt.IsZero() {
			if ts, err := SnowflakeTime(lt.IDStr); err == nil {
				t.CreatedAt = ts
			}
		}
		if u, ok := raw.GlobalObjects.Users[lt.UserIDStr]; ok {
			t.AuthorHandle = u.ScreenName
			t.AuthorName = u.Name
		}
		tweets = append(tweets, t)
	}
	sortNewestFirst(tweets)
	return tweets, nil
}

// collectListedTweets gathers the ids of tweets the timeline lists, in ANY
// entry shape (addEntries entries, timelineModule items, replaceEntry), by
// walking the decoded JSON for {"tweet": {"id": ...}} items. Promoted content is
// skipped: an entry whose entryId starts with "promoted", or any object carrying
// promotedMetadata, contributes nothing.
func collectListedTweets(v any, out map[string]struct{}) {
	switch n := v.(type) {
	case map[string]any:
		if id, _ := n["entryId"].(string); strings.HasPrefix(id, "promoted") {
			return
		}
		if pm, ok := n["promotedMetadata"]; ok && pm != nil {
			return
		}
		if t, ok := n["tweet"].(map[string]any); ok {
			if id, _ := t["id"].(string); id != "" {
				if pm, ok := t["promotedMetadata"]; !ok || pm == nil {
					out[id] = struct{}{}
				}
			}
		}
		for _, c := range n {
			collectListedTweets(c, out)
		}
	case []any:
		for _, c := range n {
			collectListedTweets(c, out)
		}
	}
}

var (
	deviceFollowNoTimeline     atomic.Int64
	deviceFollowNoTimelineOnce sync.Once
)

// noteDeviceFollowWithoutTimeline counts responses that carry no timeline (so
// the listed-only filter could not run) and logs the first one.
func noteDeviceFollowWithoutTimeline() {
	deviceFollowNoTimeline.Add(1)
	deviceFollowNoTimelineOnce.Do(func() {
		slog.Warn("device_follow: response has no timeline; cannot tell feed items from referenced tweets, returning all")
	})
}

func hasJSONValue(r json.RawMessage) bool { return len(r) > 0 && string(r) != "null" }

// sortNewestFirst orders tweets by descending snowflake id (numeric: a longer
// id is larger, equal lengths compare lexically).
func sortNewestFirst(ts []*model.Tweet) {
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i].ID, ts[j].ID
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		return a > b
	})
}

// xwebTweetResult is a tweet in the x-web (Vite/Relay) response shape: text and
// timestamp live under details, the author under core.user_results, and
// legacy only keeps conversation metadata.
type xwebTweetResult struct {
	RestID  string `json:"rest_id"`
	Details struct {
		FullText    string `json:"full_text"`
		CreatedAtMs int64  `json:"created_at_ms"`
	} `json:"details"`
	Legacy struct {
		FullText  string `json:"full_text"`
		UserIDStr string `json:"user_id_str"`
	} `json:"legacy"`
	Core struct {
		UserResults struct {
			Result userResult `json:"result"`
		} `json:"user_results"`
	} `json:"core"`
	Tweet *xwebTweetResult `json:"tweet"`
}

// xwebTimeline is the timeline of the x-web shape: snake_case entry keys and
// the item content nested one level deeper (entries[].content.content).
type xwebTimeline struct {
	Instructions []struct {
		Entries []struct {
			EntryID string `json:"entry_id"`
			Content struct {
				PromotedMetadata json.RawMessage `json:"promotedMetadata"`
				Content          struct {
					PromotedMetadata json.RawMessage `json:"promotedMetadata"`
					TypeName         string          `json:"__typename"`
					TweetResults     struct {
						Result xwebTweetResult `json:"result"`
					} `json:"tweet_results"`
				} `json:"content"`
			} `json:"content"`
		} `json:"entries"`
	} `json:"instructions"`
}

// TweetNotifications parses the tweetNotificationsQuery response.
// Root: data.viewer_v2.user_results.result.tweet_notification_timeline.timeline.
// The current (x-web) answer uses snake_case entries with the item nested under
// content.content; the earlier URT shape (entryId / itemContent) is still
// accepted so a rollback of the server side does not blind the fallback.
func TweetNotifications(body []byte, opts ...Option) ([]*model.Tweet, string, error) {
	o := newOptions(opts)
	var env struct {
		Data struct {
			ViewerV2 struct {
				UserResults struct {
					Result struct {
						TweetNotificationTimeline struct {
							Timeline xwebTimeline `json:"timeline"`
						} `json:"tweet_notification_timeline"`
					} `json:"result"`
				} `json:"user_results"`
			} `json:"viewer_v2"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, "", fmt.Errorf("unmarshal %s: %w", TweetNotificationsOp, err)
	}
	if len(env.Errors) > 0 {
		return nil, "", fmt.Errorf("twitter API error: %s", env.Errors[0].Message)
	}
	tl := env.Data.ViewerV2.UserResults.Result.TweetNotificationTimeline.Timeline
	var tweets []*model.Tweet
	for _, ins := range tl.Instructions {
		for _, e := range ins.Entries {
			if e.Content.Content.TypeName != "TimelineTweet" || strings.HasPrefix(e.EntryID, "promoted") ||
				hasJSONValue(e.Content.PromotedMetadata) || hasJSONValue(e.Content.Content.PromotedMetadata) {
				continue
			}
			r := e.Content.Content.TweetResults.Result
			for r.RestID == "" && r.Tweet != nil {
				r = *r.Tweet
			}
			if r.RestID == "" {
				o.drop(DropNoID)
				slog.Debug("skip notification tweet without rest_id", slog.String("entry", e.EntryID))
				continue
			}
			author := r.Core.UserResults.Result
			text := r.Details.FullText
			if text == "" {
				text = r.Legacy.FullText
			}
			authorID := author.RestID
			if authorID == "" {
				authorID = r.Legacy.UserIDStr
			}
			t := &model.Tweet{ID: r.RestID, AuthorID: authorID, AuthorHandle: author.screenName(), AuthorName: author.displayName(), Text: text}
			if r.Details.CreatedAtMs > 0 {
				t.CreatedAt = time.UnixMilli(r.Details.CreatedAtMs).UTC()
			}
			tweets = append(tweets, t)
		}
	}
	if len(tweets) == 0 {
		return tweetNotificationsURT(body, opts)
	}
	return tweets, "", nil
}

// tweetNotificationsURT parses the earlier URT-shaped answer through the
// shared timeline machinery (its recursive fallback covers a rotated wrapper key).
func tweetNotificationsURT(body []byte, opts []Option) ([]*model.Tweet, string, error) {
	return timelineTweetParse(body, TweetNotificationsOp, func(data json.RawMessage) timelineObj {
		var r struct {
			ViewerV2 struct {
				UserResults struct {
					Result struct {
						TweetNotificationTimeline struct {
							Timeline timelineObj `json:"timeline"`
						} `json:"tweet_notification_timeline"`
					} `json:"result"`
				} `json:"user_results"`
			} `json:"viewer_v2"`
		}
		_ = json.Unmarshal(data, &r)
		return r.ViewerV2.UserResults.Result.TweetNotificationTimeline.Timeline
	}, opts)
}
