package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/anatolykoptev/go-twitter/internal/parse"
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
	tweets, err := parse.DeviceFollow(body, c.metrics.parseDropOption(deviceFollowEndpoint))
	if err != nil {
		return nil, fmt.Errorf("parse DeviceFollow: %w", err)
	}
	c.metrics.recordFreshness(deviceFollowEndpoint, tweets)
	return tweets, nil
}

// tweetNotificationsOp is the GraphQL operation behind the web client's
// notification-bell timeline.
const tweetNotificationsOp = parse.TweetNotificationsOp

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
	tweets, _, err := parse.TweetNotifications(body, c.metrics.parseDropOption(tweetNotificationsOp))
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
