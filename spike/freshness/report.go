package main

// Report recomputes every statistic from observations.jsonl — never from the
// live tracker — so it works post-hoc and on partial runs.
//
// Provably-late rule (as specified, 1 s clock-offset slack): for tweet T first
// seen by arm A in poll k, let P be A's most recent COVERING poll before k (one
// that could have shown T). T is provably late for A iff P.recv > created(T) +
// 1000ms — i.e. the tweet already existed when the previous covering response
// arrived, yet was absent from it.
//
// Miss rule: T seen by ≥1 other arm; A had ≥1 covering poll for T's author
// inside (firstSightElsewhere, firstSightElsewhere+10min] and never saw T
// itself → miss10m. missTotal = same but across the whole run window.
//
// Latency for the per-arm table is firstSeen − created for originals only.
// Paired diffs are firstSeen_arm − firstSeen_usertweets on tweets both saw.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// armReplay is one arm's timeline rebuilt from the observation log.
type armReplay struct {
	polls  []replayPoll
	seen   map[string]*replayTweet
	ticks  []int64 // inter-poll gaps (sent deltas)
	errs   int
	err429 int
	stops  []string
}

type replayPoll struct {
	sent, recv int64
	covers     map[string]bool
	ids        map[string]bool
	err        string
}

type replayTweet struct {
	id, handle, typ  string
	created          int64
	firstSeen        int64
	lastMiss         int64
	prevCoverAtFirst int64 // recv of the covering poll right before first-sight
}

func runReport(out string) error {
	path := filepath.Join(out, "observations.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	arms := map[string]*armReplay{}
	targetHandles := map[string]bool{}
	var windowStart, windowEnd int64

	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var o observation
		if json.Unmarshal([]byte(line), &o) != nil {
			continue
		}
		if windowStart == 0 || o.Sent < windowStart {
			windowStart = o.Sent
		}
		if o.Recv > windowEnd {
			windowEnd = o.Recv
		}
		a := arms[o.Arm]
		if a == nil {
			a = &armReplay{seen: map[string]*replayTweet{}}
			arms[o.Arm] = a
		}
		p := replayPoll{sent: o.Sent, recv: o.Recv, err: o.Err,
			covers: map[string]bool{}, ids: map[string]bool{}}
		for _, c := range o.Covers {
			p.covers[c] = true
		}
		inResp := map[string]bool{}
		for _, t := range o.Tweets {
			p.ids[t.ID] = true
			inResp[t.ID] = true
			created := snowflakeMs(t.ID)
			if t.Type == "original" {
				targetHandles[strings.ToLower(t.Handle)] = true
			}
			if rt := a.seen[t.ID]; rt == nil {
				// previous covering poll for this author = most recent poll
				// before this one whose covers contained the author
				prev := int64(0)
				for i := len(a.polls) - 1; i >= 0; i-- {
					if a.polls[i].covers[strings.ToLower(t.Handle)] {
						prev = a.polls[i].recv
						break
					}
				}
				a.seen[t.ID] = &replayTweet{
					id: t.ID, handle: strings.ToLower(t.Handle), typ: t.Type,
					created: created, firstSeen: o.Recv, lastMiss: prev,
					prevCoverAtFirst: prev,
				}
			}
		}
		for _, rt := range a.seen {
			if !inResp[rt.id] && p.covers[rt.handle] {
				rt.lastMiss = o.Recv
			}
		}
		if o.Err != "" {
			a.errs++
			if strings.Contains(o.Err, "429") || strings.Contains(o.Err, "88") {
				a.err429++
			}
		}
		if n := len(a.polls); n > 0 {
			a.ticks = append(a.ticks, o.Sent-a.polls[n-1].sent)
		}
		a.polls = append(a.polls, p)
	}

	return renderReport(out, arms, targetHandles, windowStart, windowEnd)
}

func snowflakeMs(id string) int64 {
	// same formula as twitter.SnowflakeTime, no error path needed for report
	var n uint64
	if _, err := fmt.Sscan(id, &n); err != nil {
		return 0
	}
	return int64(n>>22) + 1288834974657
}

// firstSight is, for each original tweet, the earliest time ANY arm saw it.
func firstSights(arms map[string]*armReplay) map[string]int64 {
	out := map[string]int64{}
	for _, a := range arms {
		for id, t := range a.seen {
			if t.typ != "original" || t.firstSeen == 0 {
				continue
			}
			if out[id] == 0 || t.firstSeen < out[id] {
				out[id] = t.firstSeen
			}
		}
	}
	return out
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

func sortedCopy(v []int64) []int64 {
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s
}

func renderReport(out string, arms map[string]*armReplay, targets map[string]bool, windowStart, windowEnd int64) error {
	var b strings.Builder
	b.WriteString("# Freshness spike report\n\n")
	b.WriteString(fmt.Sprintf("Generated %s\n\n", time.Now().UTC().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("Measurement window: %s → %s (%.0f min).\n",
		time.UnixMilli(windowStart).UTC().Format(time.RFC3339),
		time.UnixMilli(windowEnd).UTC().Format(time.RFC3339),
		float64(windowEnd-windowStart)/60000))
	b.WriteString("Only originals whose snowflake time falls inside the window are\n")
	b.WriteString("counted; first-page stragglers created before the window are excluded.\n\n")

	first := firstSights(arms)
	coveredPolls := func(a *armReplay, handle string, from, to int64) int {
		n := 0
		for _, p := range a.polls {
			if p.err == "" && p.covers[handle] && p.recv > from && p.recv <= to {
				n++
			}
		}
		return n
	}
	createdOf := func(id string) int64 {
		for _, a2 := range arms {
			if t2 := a2.seen[id]; t2 != nil {
				return t2.created
			}
		}
		return 0
	}

	b.WriteString("## Per-arm summary (originals created inside the window)\n\n")
	b.WriteString("| arm | polls | errs(429) | tick med ms | n | p50 ms | p90 | p99 | max | provably late | bound p50 | bound p90 | seen@1st poll | miss<10m | never seen | excluded old |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, name := range sortedKeys(arms) {
		a := arms[name]
		var lats, bounds []int64
		var late, miss10, never, old, firstPoll int
		for _, t := range a.seen {
			if t.typ != "original" || t.created == 0 {
				continue
			}
			if t.created < windowStart {
				old++
				continue
			}
			lats = append(lats, t.firstSeen-t.created)
			if t.prevCoverAtFirst > t.created+provablyLateSlackMs {
				late++
			}
			if t.prevCoverAtFirst > 0 {
				bounds = append(bounds, t.firstSeen-t.prevCoverAtFirst)
			} else {
				firstPoll++
			}
		}
		// misses: in-window originals any OTHER arm saw but this arm didn't
		for id, fs := range first {
			if a.seen[id] != nil {
				continue
			}
			if c := createdOf(id); c > 0 && c < windowStart {
				continue
			}
			handle := ""
			for _, a2 := range arms {
				if t2 := a2.seen[id]; t2 != nil {
					handle = t2.handle
				}
			}
			if handle == "" {
				continue
			}
			if coveredPolls(a, handle, fs, fs+10*60*1000) > 0 {
				miss10++
			}
			if coveredPolls(a, handle, 0, 1<<62) > 0 {
				never++
			}
		}
		sl := sortedCopy(lats)
		sb := sortedCopy(bounds)
		tick := percentile(sortedCopy(a.ticks), 0.5)
		fmt.Fprintf(&b, "| %s | %d | %d(%d) | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d |\n",
			name, len(a.polls), a.errs, a.err429, tick, len(lats),
			percentile(sl, 0.5), percentile(sl, 0.9), percentile(sl, 0.99),
			last(sl), late,
			percentile(sb, 0.5), percentile(sb, 0.9), firstPoll,
			miss10, never, old)
	}
	b.WriteString("\n'bound' = firstSeen − recv of the previous covering poll (the bracket\n")
	b.WriteString("width the truth lies in); 'seen@1st poll' = no covering poll before first\n")
	b.WriteString("sight, so latency is unbounded below by that arm's coverage start.\n")

	b.WriteString("\n## Paired diffs vs usertweets (firstSeen_arm − firstSeen_C, ms)\n\n")
	base := arms["usertweets"]
	if base != nil {
		b.WriteString("| arm | n pairs | median | p25 | p75 | p90 |\n|---|---|---|---|---|---|\n")
		for _, name := range sortedKeys(arms) {
			if name == "usertweets" {
				continue
			}
			var diffs []int64
			for id, t := range arms[name].seen {
				if t.typ != "original" || t.created < windowStart {
					continue
				}
				bt := base.seen[id]
				if bt == nil {
					continue
				}
				diffs = append(diffs, t.firstSeen-bt.firstSeen)
			}
			ds := sortedCopy(diffs)
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d |\n",
				name, len(ds), percentile(ds, 0.5), percentile(ds, 0.25),
				percentile(ds, 0.75), percentile(ds, 0.9))
		}
	} else {
		b.WriteString("(no usertweets observations)\n")
	}

	b.WriteString("\n## Observed X rate limits\n\n")
	b.WriteString("(from ratelimit.csv — max x-rate-limit-limit observed, min/max remaining)\n\n")
	b.WriteString("| endpoint | observed limit | min remaining | max remaining |\n|---|---|---|---|\n")
	rlRows, _ := os.ReadFile(filepath.Join(out, "ratelimit.csv"))
	type rlAgg struct{ limit, rmin, rmax int64 }
	agg := map[string]*rlAgg{}
	for _, line := range strings.Split(string(rlRows), "\n")[1:] {
		f := strings.Split(line, ",")
		if len(f) < 5 || f[1] == "endpoint" {
			continue
		}
		ep := f[1]
		var lim, rmin, rmax int64
		fmt.Sscan(f[2], &lim)
		fmt.Sscan(f[3], &rmin)
		fmt.Sscan(f[4], &rmax)
		a := agg[ep]
		if a == nil {
			a = &rlAgg{rmin: 1 << 60}
			agg[ep] = a
		}
		if lim > a.limit {
			a.limit = lim
		}
		if rmin >= 0 && rmin < a.rmin {
			a.rmin = rmin
		}
		if rmax > a.rmax {
			a.rmax = rmax
		}
	}
	for _, ep := range sortedKeysG(agg) {
		a := agg[ep]
		rmin := a.rmin
		if rmin == 1<<60 {
			rmin = -1
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", ep, a.limit, rmin, a.rmax)
	}

	b.WriteString("\n## Sizing — accounts needed for 2000 authors\n\n")
	b.WriteString("request budget per account = 0.4 × observed-limit / 15 min window.\n")
	b.WriteString("authors/req at 2000-author scale is the COVERAGE cap, not the spike's 8:\n")
	b.WriteString("home: one account follows all 2000 (follow cap ~5000, X docs, unverified)\n")
	b.WriteString("→ one poll covers everyone; list: one list holds 2000 (5000 members/list,\n")
	b.WriteString("X docs) → one poll covers everyone; search: ~50 from:-clauses per query\n")
	b.WriteString("(query length bound, unverified) → ~40 queries per full scan;\n")
	b.WriteString("usertweets: 1 author per request, always.\n")
	b.WriteString("list-100-shards = 2000 authors split into ~100-author lists (20 lists),\n")
	b.WriteString("one poll per list per tick — the fan-in variant whose page depth still\n")
	b.WriteString("covers a whole tick. Lists/account cap = 1000 (X docs).\n\n")
	b.WriteString("CAVEAT: these are RATE-LIMIT floors. A fan-in poll returns only the\n")
	b.WriteString("newest ~20–40 entries; at 2000 authors tweets age past page depth within\n")
	b.WriteString("one tick, so home/list need per-poll pagination (extra requests, not\n")
	b.WriteString("counted) or they silently drop tweets. Search does not have this problem\n")
	b.WriteString("(query-then-index, not a materialized page).\n\n")
	b.WriteString("| arm | endpoint | authors/req | obs limit | req/s@40% | accts@2s | accts@5s | accts@10s |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	plan := []struct {
		arm, ep string
		apreq   int
	}{
		{"home", "HomeLatestTimeline", 2000},
		{"list", "ListLatestTweetsTimeline", 2000},
		{"list-100-shards", "ListLatestTweetsTimeline", 100},
		{"search", "SearchTimeline", 50},
		{"usertweets", "UserTweets", 1},
	}
	for _, p := range plan {
		lim := int64(0)
		if a := agg[p.ep]; a != nil {
			lim = a.limit
		}
		rps := 0.4 * float64(lim) / 900.0
		cells := ""
		for _, tick := range []float64{2, 5, 10} {
			if rps <= 0 {
				cells += " n/a |"
				continue
			}
			// authors one account covers at this tick = rps*tick*authorsPerReq
			covers := rps * tick * float64(p.apreq)
			accts := math.Ceil(2000.0 / covers)
			if accts < 1 {
				accts = 1
			}
			cells += fmt.Sprintf(" %.0f |", accts)
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %.3f |%s\n",
			p.arm, p.ep, p.apreq, lim, rps, cells)
	}

	b.WriteString("\n## Method notes\n\n")
	b.WriteString("- Provably late: previous covering poll recv > created+1000ms.\n")
	b.WriteString("- Types: 'original' = authored by a target handle; replies/quotes by a\n")
	b.WriteString("  target are counted as originals (Tweet model carries no such flags).\n")
	b.WriteString("- 'foreign' rows in tweets.csv are retweets/curation seen in timelines.\n")

	fpath := filepath.Join(out, "report.md")
	if err := os.WriteFile(fpath, []byte(b.String()), 0o600); err != nil {
		return err
	}
	fmt.Println(b.String())
	slog.Info("report written", "path", fpath)
	return nil
}

func sortedKeys(m map[string]*armReplay) []string {
	return sortedKeysG(m)
}

func sortedKeysG[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func last(s []int64) int64 {
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1]
}
