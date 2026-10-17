package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	twitter "github.com/anatolykoptev/go-twitter"
)

// provablyLateSlack is the clock-offset slack the tester used (1 s): a poll is
// "provably late" for a tweet iff the arm's previous covering poll's response
// arrived MORE than (created + slack) and did not contain the tweet.
const provablyLateSlackMs = 1000

// arm is one concurrently-polled source.
type arm struct {
	name    string            // "home","list","search","usertweets","devfollow"
	account string            // account label for the CSV ("EvalikFulvik"...)
	op      string            // go-twitter endpoint name for rate-limit attribution
	targets map[string]string // author-lower -> user ID (target set)

	mu         sync.Mutex
	coverSince map[string]int64 // author-lower -> earliest sent-ms a poll covers it
	prevCover  map[string]int64 // author-lower -> recv-ms of previous covering poll
	seen       map[string]*tweetRow
	polls      int
	errs429    int
	errsAuth   int
	stopped    bool
	stopReason string
	running    bool
}

func newArm(name, account, op string, targets map[string]string) *arm {
	return &arm{
		name:       name,
		account:    account,
		op:         op,
		targets:    targets,
		coverSince: map[string]int64{},
		prevCover:  map[string]int64{},
		seen:       map[string]*tweetRow{},
	}
}

// setCoverSince declares that polls sent at/after ms cover the author.
func (a *arm) setCoverSince(authorLower string, ms int64) {
	a.mu.Lock()
	a.coverSince[authorLower] = ms
	a.mu.Unlock()
}

// classify picks the CSV type column value: "original" for tweets authored by a
// target handle, "foreign" otherwise (arrived via retweet/curation).
// NOTE: go-twitter's Tweet model carries no reply/quote/retweet flags — replies
// and quotes by a target handle land in "original". (Library gap, see report.)
func (a *arm) classify(t *twitter.Tweet) string {
	if _, ok := a.targets[strings.ToLower(t.AuthorHandle)]; ok {
		return "original"
	}
	return "foreign"
}

// record ingests one poll outcome. covers is the set of author-lowers this
// specific poll could have shown (arm C: only the polled author; A/B/D: all
// authors whose coverSince <= sent).
func (a *arm) record(o observation, covers []string, tweets []obsTweet) (nNew int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.polls++
	inResp := make(map[string]bool, len(tweets))
	for _, t := range tweets {
		inResp[t.ID] = true
		if _, ok := a.seen[t.ID]; ok {
			continue
		}
		created := int64(0)
		if ts, err := twitter.SnowflakeTime(t.ID); err == nil {
			created = ts.UnixMilli()
		}
		row := &tweetRow{
			ID: t.ID, Handle: t.Handle, Type: t.Type, CreatedMs: created,
			Arm: a.name, Account: a.account,
			// The previous covering poll necessarily lacked this tweet
			// (first-seen is the first poll containing it), so its recv is
			// the candidate last-miss — provably-late iff it postdates
			// created+slack, which the report evaluates.
			LastMiss:  a.prevCover[strings.ToLower(t.Handle)],
			FirstSeen: o.Recv,
		}
		a.seen[t.ID] = row
		nNew++
	}
	// Latest miss for already-tracked tweets absent from this covering poll.
	covered := map[string]bool{}
	for _, c := range covers {
		covered[c] = true
		a.prevCover[c] = o.Recv
	}
	for id, row := range a.seen {
		if !inResp[id] && covered[strings.ToLower(row.Handle)] {
			row.LastMiss = o.Recv
		}
	}
	return nNew
}

func (a *arm) stop(reason string) {
	a.mu.Lock()
	a.stopped = true
	a.stopReason = reason
	a.mu.Unlock()
	slog.Warn("arm stopped", "arm", a.name, "reason", reason)
}

// coversOf returns the author-lowers a poll sent at ms covers.
func (a *arm) coversOf(sentMs int64) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.coverSince))
	for h, since := range a.coverSince {
		if since <= sentMs {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// pollOnce performs one timed call and folds it into the tracker + writer.
// Returns the tweets for callers that chain logic on them.
func (a *arm) pollOnce(ctx context.Context, w *writer, seq int, fetch func(context.Context) ([]obsTweet, error)) []obsTweet {
	sent := time.Now()
	tweets, err := fetch(ctx)
	recv := time.Now()
	o := observation{
		Arm: a.name, Op: a.op, Seq: seq,
		Sent: sent.UnixMilli(), Recv: recv.UnixMilli(),
	}
	var covers []string
	var nNew int
	if err != nil {
		o.Err = err.Error()
		a.noteErr(err)
	} else {
		covers = a.coversOf(o.Sent)
		o.Covers = covers
		o.Tweets = tweets
		nNew = a.record(o, covers, tweets)
	}
	w.recordObservation(o, nNew)
	return tweets
}

// noteErr implements the spec's stop rule: 429/403/326/401 stop the arm.
func (a *arm) noteErr(err error) {
	switch {
	case twitter.IsRateLimited(err):
		a.errs429++
		a.stop("rate limited (429/code 88)")
	case twitter.IsLocked(err):
		a.stop("account locked (code 326)")
	case twitter.IsForbidden(err), twitter.IsSuspended(err):
		a.stop("forbidden/suspended (403)")
	case twitter.IsAuthExpired(err):
		a.errsAuth++
		a.stop("auth expired (401/code 32)")
	case twitter.IsBanned(err):
		a.stop("account banned (code 88 durable)")
	default:
		// transient (transport, endpoint, overloaded, csrf retry-exhausted):
		// count but keep polling — an outage is itself a measurement.
		a.errsAuth++
	}
}

// pacer computes inter-poll spacing from the X-observed per-endpoint limit:
// interval = window / (fraction * observedLimit), clamped.
type pacer struct {
	sampler  *rlSampler
	op       string
	fraction float64
	minTick  time.Duration
	maxTick  time.Duration
	fallback time.Duration // until a limit is observed
}

func (p *pacer) interval() time.Duration {
	lim := p.sampler.observedLimit(p.op)
	if lim <= 0 {
		return p.fallback
	}
	iv := time.Duration(float64(15*time.Minute) / (p.fraction * float64(lim)))
	if iv < p.minTick {
		return p.minTick
	}
	if iv > p.maxTick {
		return p.maxTick
	}
	return iv
}

// sleepCtx waits d or ctx, with ±20% jitter to decorrelate arms.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	j := 1 + (rand.Float64()*0.4 - 0.2)
	t := time.NewTimer(time.Duration(float64(d) * j))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// loop is the generic per-endpoint poller used by home, list, search and
// devfollow: fetch once per tick until ctx ends or the arm stops.
func (a *arm) loop(ctx context.Context, w *writer, p *pacer, fetch func(context.Context) ([]obsTweet, error)) {
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
	seq := 0
	for ctx.Err() == nil {
		a.mu.Lock()
		stopped := a.stopped
		a.mu.Unlock()
		if stopped {
			return
		}
		seq++
		a.pollOnce(ctx, w, seq, fetch)
		if !sleepCtx(ctx, p.interval()) {
			return
		}
	}
}

// userTweetsLoop is arm C: round-robin one request per author per tick; each
// poll covers exactly the polled author.
func (a *arm) userTweetsLoop(ctx context.Context, w *writer, p *pacer, client *twitter.Client, ids []string, idToHandle map[string]string) {
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
	seq := 0
	i := 0
	for ctx.Err() == nil {
		a.mu.Lock()
		stopped := a.stopped
		a.mu.Unlock()
		if stopped {
			return
		}
		uid := ids[i%len(ids)]
		i++
		seq++
		a.pollOnceAuthor(ctx, w, seq, idToHandle[uid], func(ctx context.Context) ([]obsTweet, error) {
			ts, err := client.GetUserTweets(ctx, uid, 20)
			if err != nil {
				return nil, err
			}
			return a.toObs(ts), nil
		})
		if !sleepCtx(ctx, p.interval()) {
			return
		}
	}
}

// toObs converts library tweets to observation rows.
func (a *arm) toObs(ts []*twitter.Tweet) []obsTweet {
	out := make([]obsTweet, 0, len(ts))
	for _, t := range ts {
		if t == nil {
			continue
		}
		out = append(out, obsTweet{ID: t.ID, Handle: t.AuthorHandle, Type: a.classify(t)})
	}
	return out
}

// pollOnceAuthor is pollOnce with explicit coverage (arm C: each poll covers
// exactly the polled author).
func (a *arm) pollOnceAuthor(ctx context.Context, w *writer, seq int, authorLower string, fetch func(context.Context) ([]obsTweet, error)) []obsTweet {
	sent := time.Now()
	tweets, err := fetch(ctx)
	recv := time.Now()
	o := observation{
		Arm: a.name, Op: a.op, Seq: seq,
		Sent: sent.UnixMilli(), Recv: recv.UnixMilli(),
	}
	var nNew int
	if err != nil {
		o.Err = err.Error()
		a.noteErr(err)
	} else {
		o.Covers = []string{authorLower}
		o.Tweets = tweets
		nNew = a.record(o, o.Covers, tweets)
	}
	w.recordObservation(o, nNew)
	return tweets
}

// jitterRange sleeps a random duration in [lo, hi] or ctx-cancels.
func jitterRange(ctx context.Context, lo, hi time.Duration) bool {
	d := lo + time.Duration(rand.Float64()*float64(hi-lo))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
