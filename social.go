package twitter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"

	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/social"
	"github.com/anatolykoptev/go-twitter/xtid"
	"go.opentelemetry.io/otel/metric"
)

const maxSocialRetries = 6

// socialEndpointFaultStop bounds the run of consecutive endpoint faults —
// each on a DIFFERENT lease — a *WithSocial call tolerates before giving up.
// An ErrEndpoint hit means the committed request shape is stale for EVERY
// account (dead queryId, missing xtid, feature mismatch), so once two
// independent credentials both fault, the fault is provably endpoint-wide and
// further leases only burn go-social's rate budget (1 unit per AcquireAccount)
// with no chance of success. Two is the cheapest possible confirmation that
// the fault is not one account's quirk.
const socialEndpointFaultStop = 2

// socialMeter holds the MeterProvider the *WithSocial helpers record to.
var socialMeter atomic.Pointer[meterProviderBox]

type meterProviderBox struct{ mp metric.MeterProvider }

// SetSocialMeterProvider sets the OpenTelemetry MeterProvider that the
// *WithSocial helpers record to (request.duration, timeline.freshness,
// relogins, proxy.failures; see docs/metrics.md). Call it once at start-up.
// nil (the default) falls back to otel.GetMeterProvider(). The per-call
// clients behind these helpers never register the pool.accounts gauge.
func SetSocialMeterProvider(mp metric.MeterProvider) {
	if mp == nil {
		socialMeter.Store(nil)
		return
	}
	socialMeter.Store(&meterProviderBox{mp: mp})
}

func socialMeterProvider() metric.MeterProvider {
	if b := socialMeter.Load(); b != nil {
		return b.mp
	}
	return nil
}

// Process-wide xtid manager shared by every per-call client of the *WithSocial
// path. Each call builds a fresh one-account Client, so a per-client manager
// would re-run the warm-page fetch and chunk scan on every call.
var (
	socialXtidMu  sync.Mutex
	socialXtidMgr *xtid.Manager
)

// sharedSocialXtid returns the shared manager, creating it on first use. Its
// guest-fallback fetcher uses a dedicated direct client; authenticated warm
// fetches still go through the requesting account's own client and cookie
// (warmFetcher), never a captured one. Returns nil when the fetch client cannot
// be built, in which case NewClient falls back to a per-client manager.
func sharedSocialXtid() *xtid.Manager {
	socialXtidMu.Lock()
	defer socialXtidMu.Unlock()
	if socialXtidMgr != nil {
		return socialXtidMgr
	}
	bc, err := stealth.NewClient(stealth.WithHeaderOrder(twitterHeaderOrder))
	if err != nil {
		slog.Warn("twitter: shared xtid fetch client failed, using per-client managers", slog.Any("error", err))
		return nil
	}
	mgr := xtid.NewManager(&bundle.StealthFetcher{Client: bc, UserAgent: defaultUserAgent})
	mgr.SetInitFailedHook(func(err error) {
		slog.Warn("pool alert", slog.String("topic", "xtid.init_failed"), slog.Any("payload", map[string]any{"error": err.Error()}))
	})
	socialXtidMgr = mgr
	return mgr
}

// testHookSocialClient, when non-nil, runs on each per-call client before fn.
// Test seam only.
var testHookSocialClient func(*Client)

// SearchWithSocial acquires accounts from go-social and searches Twitter on the
// Latest tab, retrying with different accounts on failure. go-social rotates
// accounts on each call, so retries naturally use different credentials.
func SearchWithSocial(ctx context.Context, sc *social.Client, query string, limit int) ([]*Tweet, error) {
	return withSocialAccount(ctx, sc, "SearchTimeline", func(ctx context.Context, tw *Client) ([]*Tweet, error) {
		return tw.SearchTimeline(ctx, query, limit)
	})
}

// SearchTweetsWithSocial searches tweets on an explicit tab (Top or Latest).
func SearchTweetsWithSocial(ctx context.Context, sc *social.Client, query string, product SearchProduct, limit int) ([]*Tweet, error) {
	// Validate before acquiring — a typo'd product must not burn pool retries.
	if product != SearchTop && product != SearchLatest {
		return nil, fmt.Errorf("SearchTweets: invalid product %q (want Top or Latest)", product)
	}
	return withSocialAccount(ctx, sc, "SearchTimeline", func(ctx context.Context, tw *Client) ([]*Tweet, error) {
		return tw.SearchTweets(ctx, query, product, limit)
	})
}

// SearchUsersWithSocial searches the People tab.
func SearchUsersWithSocial(ctx context.Context, sc *social.Client, query string, limit int) ([]*TwitterUser, error) {
	return withSocialAccount(ctx, sc, "SearchTimeline", func(ctx context.Context, tw *Client) ([]*TwitterUser, error) {
		return tw.SearchUsers(ctx, query, limit)
	})
}

// GetUserWithSocial fetches a profile by handle.
func GetUserWithSocial(ctx context.Context, sc *social.Client, handle string) (*TwitterUser, error) {
	return withSocialAccount(ctx, sc, "UserByScreenName", func(ctx context.Context, tw *Client) (*TwitterUser, error) {
		return tw.GetUserByScreenName(ctx, handle)
	})
}

// GetUserTweetsWithSocial resolves the handle and fetches its recent tweets —
// two GraphQL calls inside one account attempt.
func GetUserTweetsWithSocial(ctx context.Context, sc *social.Client, handle string, limit int) ([]*Tweet, error) {
	return withSocialAccount(ctx, sc, "UserTweets", func(ctx context.Context, tw *Client) ([]*Tweet, error) {
		u, err := tw.GetUserByScreenName(ctx, handle)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", handle, err)
		}
		return tw.GetUserTweets(ctx, u.ID, limit)
	})
}

// GetTweetConversationWithSocial fetches the focal tweet and its conversation
// entries (replies/ancestors in page order).
func GetTweetConversationWithSocial(ctx context.Context, sc *social.Client, tweetID string) (*Tweet, []*Tweet, error) {
	type conv struct {
		focal   *Tweet
		replies []*Tweet
	}
	res, err := withSocialAccount(ctx, sc, "TweetDetail", func(ctx context.Context, tw *Client) (conv, error) {
		f, r, err := tw.GetTweetConversation(ctx, tweetID)
		return conv{f, r}, err
	})
	return res.focal, res.replies, err
}

// withSocialAccount runs fn on a client built from a freshly acquired account,
// retrying across accounts on failure — the acquire→call→ReportUsage loop
// SearchWithSocial grew organically, generalized.
func withSocialAccount[T any](ctx context.Context, sc *social.Client, op string, fn func(context.Context, *Client) (T, error)) (T, error) {
	var lastErr error
	var zero T
	endpointFaults := 0
	for attempt := range maxSocialRetries {
		res, err := tryWithAccount(ctx, sc, op, fn)
		if err == nil {
			return res, nil
		}
		if errors.Is(err, ErrNotFound) {
			return zero, err
		}
		if errors.Is(err, ErrEndpoint) {
			endpointFaults++
			if endpointFaults >= socialEndpointFaultStop {
				// Two different leases hit the same endpoint fault: the
				// request shape is dead for every account in the pool —
				// more leases only burn the go-social rate budget.
				return zero, err
			}
		} else {
			endpointFaults = 0
		}
		lastErr = err
		slog.Warn("social request attempt failed, retrying",
			slog.Int("attempt", attempt+1),
			slog.Int("max", maxSocialRetries),
			slog.Any("error", err))
	}
	return zero, fmt.Errorf("all %d accounts failed: %w", maxSocialRetries, lastErr)
}

// tryWithAccount acquires one account, runs fn, and reports the result.
// The Account is built fresh for every attempt, so the per-account
// header-less-88 count (see Account.noteRateLimited88) never reaches the
// escalation threshold on this path: a leased account that keeps answering 88
// surfaces as a call error and is reported to the credential service as
// "auth_error", never "banned".
func tryWithAccount[T any](ctx context.Context, sc *social.Client, op string, fn func(context.Context, *Client) (T, error)) (T, error) {
	var zero T
	creds, err := sc.AcquireAccountFor(ctx, "twitter", op)
	if err != nil {
		return zero, fmt.Errorf("acquire account: %w", err)
	}

	acc := &Account{
		Username:  creds.Credentials["username"],
		AuthToken: creds.Credentials["auth_token"],
		CT0:       creds.Credentials["ct0"],
		// The lease's proxy belongs to the ACCOUNT, not the client: setting
		// acc.Proxy (instead of ClientConfig.DefaultProxy) makes NewClient
		// build a per-account transport for it and wires the pool-path
		// proxy-failure accounting — a dead proxy answering 5xx or refusing
		// the tunnel marks itself down instead of silently passing as an
		// account-layer fault.
		Proxy: creds.Proxy,

		credsAuthoritative: true, // freshly leased: a stale session file must not override them
	}

	// report sends the usage verdict together with the last rate-limit window
	// this lease observed (op's endpoint when seen, else the latest endpoint),
	// so the credential service can steer later leases by endpoint budget. It
	// carries the window on success AND failure; with no header seen it sends
	// the legacy body.
	report := func(status string) {
		var rl *social.RateLimit
		if ep, st, ok := acc.latestRateLimit(op); ok {
			rl = &social.RateLimit{Endpoint: ep, Limit: st.limit, Remaining: st.remaining, Reset: st.resetAt.Unix()}
		}
		_ = sc.ReportUsageWithRateLimit(ctx, "twitter", creds.ID, status, rl)
	}

	tw, err := NewClient(ClientConfig{
		Accounts:  []*Account{acc},
		RateLimit: ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute},

		MeterProvider: socialMeterProvider(),
		sharedXtid:    sharedSocialXtid(),
		skipPoolGauge: true,
	})
	if err != nil {
		report("auth_error")
		return zero, fmt.Errorf("create client for %s: %w", acc.Username, err)
	}

	defer func() { _ = tw.Close() }()
	if testHookSocialClient != nil {
		testHookSocialClient(tw)
	}

	res, err := fn(ctx, tw)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The account completed the request fine — the resource is just
			// absent. Healthy account, deterministic miss: report success and
			// propagate untouched so withSocialAccount doesn't retry.
			report("success")
			return zero, err
		}
		if errors.Is(err, ErrEndpoint) {
			// Endpoint fault (#61): a 404/GraphQL-shaped-400 means the
			// committed request shape is stale for EVERY account — the leased
			// credential is innocent. Report success (never auth_error, which
			// would poison a healthy account in the shared pool) and let
			// withSocialAccount rotate to the next lease inside its budget.
			report("success")
			return zero, fmt.Errorf("%s: %w", acc.Username, err)
		}
		if errors.Is(err, ErrOverloaded) {
			// A 5xx/LoadShed is X-side — but NOT proof the credential worked:
			// a leased account whose residential proxy answers 502/503/504 for
			// the tunnel surfaces exactly this class. go-social's report
			// vocabulary (success / auth_error / rate_limited / banned) has no
			// neutral status — "success" would reset ConsecutiveFails and keep
			// a dead proxy leased forever — so make no report at all and let
			// withSocialAccount rotate to the next lease.
			return zero, fmt.Errorf("%s: %w", acc.Username, err)
		}
		report("auth_error")
		return zero, fmt.Errorf("%s: %w", acc.Username, err)
	}

	report("success")
	return res, nil
}
