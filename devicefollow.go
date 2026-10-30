package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// deviceFollowURL is the legacy endpoint serving the merged timeline of new
// tweets from every account the viewer enabled device notifications ("bell")
// for: one feed covering all belled authors.
// A var (not const) only so tests can point it at a fake server.
var deviceFollowURL = "https://x.com/i/api/2/notifications/device_follow.json"

// deviceFollowEndpoint is the rate-limit/metrics key for the REST bell source.
const deviceFollowEndpoint = "DeviceFollow"

// GetDeviceFollowTweets fetches the merged device-notifications timeline via
// the legacy REST endpoint.
func (c *Client) GetDeviceFollowTweets(ctx context.Context, count int) ([]*Tweet, error) {
	u := deviceFollowURL + "?count=" + strconv.Itoa(count)
	body, _, err := c.doGET(ctx, deviceFollowEndpoint, u)
	if err != nil {
		return nil, fmt.Errorf("DeviceFollow: %w", err)
	}
	tweets, err := parseDeviceFollow(body)
	if err != nil {
		return nil, fmt.Errorf("parse DeviceFollow: %w", err)
	}
	c.metrics.recordFreshness(deviceFollowEndpoint, tweets)
	return tweets, nil
}

// tweetNotificationsOp is the GraphQL operation behind the web client's
// notification-bell timeline.
const tweetNotificationsOp = "tweetNotificationsQuery"

// tweetNotificationsProviders are the Relay "provided variables" the operation
// declares (params.providedVariables in the x-web chunk tweet-notifications-*.js,
// reached from the logged-out shell entry-client-logged-out-*.js through its
// dynamic imports). The web client fills them from client-side feature gates;
// the server answers HTTP 422 ("must be defined") when any is missing. They only
// toggle optional response fields, so fixed values are sufficient.
//
// Variable -> gate in the web client, as of 2026-10-30:
//
//	appviewerisloggedin           -> viewer is logged in
//	translationgroktranslatedpost -> responsive_web_grok_show_grok_translated_post (default true)
//	grokanalyzepost               -> grok_xweb_analyze_post_enabled
//	noteautotranslationenabled    -> responsive_web_grok_community_note_auto_translation_is_enabled
//	liveeventpostcontextenabled   -> x_elections_post_context_enabled
//	sportspostcontextenabled      -> rweb_sports_post_context_enabled
var tweetNotificationsProviders = map[string]any{
	"__relay_internal__pv__xclientsfeaturesappviewerisloggedinrelayprovider":                   true,
	"__relay_internal__pv__xclientsfeaturestranslationgroktranslatedpostenabledrelayprovider":  true,
	"__relay_internal__pv__xclientsfeaturestweetcomponentsgrokanalyzepostrelayprovider":        false,
	"__relay_internal__pv__xclientsfeaturestranslationnoteautotranslationenabledrelayprovider": false,
	"__relay_internal__pv__liveeventpostcontextenabledrelayprovider":                           false,
	"__relay_internal__pv__sportspostcontextenabledrelayprovider":                              false,
}

// tweetNotificationsVariables builds the full variables object: the operation
// argument plus every provided variable.
func tweetNotificationsVariables(count int) map[string]any {
	v := make(map[string]any, len(tweetNotificationsProviders)+1)
	for k, val := range tweetNotificationsProviders {
		v[k] = val
	}
	v["count"] = count
	return v
}

// GetTweetNotifications fetches the same merged feed via GraphQL
// tweetNotificationsQuery — the fallback when device_follow.json is gone or
// empty. It carries its own endpoint key so a fault on one source cannot
// quarantine the account from the other.
func (c *Client) GetTweetNotifications(ctx context.Context, count int) ([]*Tweet, error) {
	url, err := tweetNotificationsURL(count)
	if err != nil {
		return nil, err
	}
	body, _, err := c.doGET(ctx, tweetNotificationsOp, url)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tweetNotificationsOp, err)
	}
	tweets, _, err := parseTweetNotifications(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", tweetNotificationsOp, err)
	}
	c.metrics.recordFreshness(tweetNotificationsOp, tweets)
	return tweets, nil
}

// tweetNotificationsURL builds the request URL. The x-web persisted query takes
// variables only: its operation declares no feature switches and the web client
// sends no `features` parameter.
func tweetNotificationsURL(count int) (string, error) {
	u, err := EndpointURL(tweetNotificationsOp)
	if err != nil {
		return "", err
	}
	vars, err := json.Marshal(tweetNotificationsVariables(count))
	if err != nil {
		return "", err
	}
	return u + "?variables=" + jsonEscape(vars), nil
}

// GetDeviceNotifications polls the merged device-notifications feed: REST
// device_follow.json, with GraphQL tweetNotificationsQuery as the fallback when
// REST is gone (HTTP 404/410 or another endpoint fault). An empty REST answer is
// a normal quiet poll and does NOT trigger the fallback — that would double the
// request volume in the steady state and hide failures. If the fallback fails
// too, both sources are broken and the joined error is returned; a cancelled or
// expired ctx is always returned as is.
func (c *Client) GetDeviceNotifications(ctx context.Context, count int) ([]*Tweet, error) {
	return deviceNotifications(ctx, count, c.GetDeviceFollowTweets, c.GetTweetNotifications, c.warnBothFailed)
}

// warnBothFailed logs the "both sources broken" condition at most once a
// minute per client, with the number of suppressed repeats.
func (c *Client) warnBothFailed(restErr, gqlErr error) {
	if ok, n := c.deviceWarn.allow(); ok {
		slog.Warn("device notifications: REST dead and GraphQL fallback failed",
			slog.String("op", tweetNotificationsOp), slog.Any("rest_error", restErr), slog.Any("error", gqlErr),
			slog.Int("suppressed", n))
	}
}

type feedFetcher func(ctx context.Context, count int) ([]*Tweet, error)

// deviceNotifications is GetDeviceNotifications with the two sources injected.
func deviceNotifications(ctx context.Context, count int, rest, gql feedFetcher, onBothFailed func(restErr, gqlErr error)) ([]*Tweet, error) {
	tweets, err := rest(ctx, count)
	if err == nil {
		return tweets, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(ctxErr, err)
	}
	var ae *APIError
	dead := IsEndpointFault(err) || (errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusGone))
	if !dead {
		return nil, err
	}
	g, gerr := gql(ctx, count)
	if gerr != nil {
		if onBothFailed != nil {
			onBothFailed(err, gerr)
		}
		return nil, errors.Join(err, gerr)
	}
	return g, nil
}

// parseDeviceFollow decodes the globalObjects shape: device_follow.json
// answers like the 1.5 API, with all tweets under globalObjects.tweets and
// their authors under globalObjects.users keyed by user id.
//
// globalObjects.tweets also carries tweets the feed merely references (the
// original behind a retweet, whose author the viewer may not follow), so when the
// timeline lists tweet entries only those are returned. Tweets come back newest
// first (descending snowflake id) with CreatedAt set.
func parseDeviceFollow(body []byte) ([]*Tweet, error) {
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
	tweets := make([]*Tweet, 0, len(raw.GlobalObjects.Tweets))
	for key, lt := range raw.GlobalObjects.Tweets {
		if lt.IDStr == "" {
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
		t := &Tweet{ID: lt.IDStr, AuthorID: lt.UserIDStr, Text: text}
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

// warnLimiter lets a repeating warning through at most once per interval and
// reports how many were suppressed meanwhile. The zero value is usable.
type warnLimiter struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
	every      time.Duration    // zero = a minute
	now        func() time.Time // nil = time.Now
}

func (w *warnLimiter) allow() (ok bool, suppressed int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	every, now := w.every, w.now
	if every == 0 {
		every = time.Minute
	}
	if now == nil {
		now = time.Now
	}
	t := now()
	if !w.last.IsZero() && t.Sub(w.last) < every {
		w.suppressed++
		return false, 0
	}
	suppressed, w.suppressed, w.last = w.suppressed, 0, t
	return true, suppressed
}

func hasJSONValue(r json.RawMessage) bool { return len(r) > 0 && string(r) != "null" }

// sortNewestFirst orders tweets by descending snowflake id (numeric: a longer
// id is larger, equal lengths compare lexically).
func sortNewestFirst(ts []*Tweet) {
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

// parseTweetNotifications parses the tweetNotificationsQuery response.
// Root: data.viewer_v2.user_results.result.tweet_notification_timeline.timeline.
// The current (x-web) answer uses snake_case entries with the item nested under
// content.content; the earlier URT shape (entryId / itemContent) is still
// accepted so a rollback of the server side does not blind the fallback.
func parseTweetNotifications(body []byte) ([]*Tweet, string, error) {
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
		return nil, "", fmt.Errorf("unmarshal %s: %w", tweetNotificationsOp, err)
	}
	if len(env.Errors) > 0 {
		return nil, "", fmt.Errorf("twitter API error: %s", env.Errors[0].Message)
	}
	tl := env.Data.ViewerV2.UserResults.Result.TweetNotificationTimeline.Timeline
	var tweets []*Tweet
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
			t := &Tweet{ID: r.RestID, AuthorID: authorID, AuthorHandle: author.screenName(), AuthorName: author.displayName(), Text: text}
			if r.Details.CreatedAtMs > 0 {
				t.CreatedAt = time.UnixMilli(r.Details.CreatedAtMs).UTC()
			}
			tweets = append(tweets, t)
		}
	}
	if len(tweets) == 0 {
		return parseTweetNotificationsURT(body)
	}
	return tweets, "", nil
}

// parseTweetNotificationsURT parses the earlier URT-shaped answer through the
// shared timeline machinery (its recursive fallback covers a rotated wrapper key).
func parseTweetNotificationsURT(body []byte) ([]*Tweet, string, error) {
	return timelineTweetParse(body, tweetNotificationsOp, func(data json.RawMessage) timelineObj {
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
	})
}
