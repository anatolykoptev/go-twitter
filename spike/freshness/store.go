package main

// Output files (all under --out, default ~/tmp/freshness-spike/):
//   observations.jsonl — one line per poll: full tweet-id list + coverage.
//                        The single source of truth the report recomputes from.
//   polls.csv          — one row per poll (sent/recv/counts/errors).
//   tweets.csv         — spec CSV, one row per tweet per arm (materialized
//                        atomically every ~60 s and at shutdown).
//   ratelimit.csv      — sampled OTel rate-limit observations per endpoint.
//   writes.csv         — every write op (follow/list/bell/undo) and its result.
//   events.log         — slog text log (human-readable run narrative).
//   state.json         — cleanup state (see state.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// obsTweet is one tweet as observed by one arm poll.
type obsTweet struct {
	ID     string `json:"id"`
	Handle string `json:"h"` // author screen_name
	Type   string `json:"ty,omitempty"`
}

// observation is one arm's single poll.
type observation struct {
	Arm    string     `json:"arm"`
	Op     string     `json:"op"`
	Seq    int        `json:"seq"`
	Sent   int64      `json:"sent"` // ms unix
	Recv   int64      `json:"recv"` // ms unix
	Covers []string   `json:"covers,omitempty"`
	Tweets []obsTweet `json:"tweets,omitempty"`
	Err    string     `json:"err,omitempty"`
	Note   string     `json:"note,omitempty"`
}

// writer fans out records to the output files.
type writer struct {
	dir   string
	mu    sync.Mutex
	obs   *os.File
	polls *os.File
	rl    *os.File
	wr    *os.File
}

func newWriter(dir string) (*writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	open := func(name string) (*os.File, error) {
		return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	w := &writer{dir: dir}
	var err error
	for _, f := range []struct {
		name string
		dst  **os.File
	}{
		{"observations.jsonl", &w.obs},
		{"polls.csv", &w.polls},
		{"ratelimit.csv", &w.rl},
		{"writes.csv", &w.wr},
	} {
		if *f.dst, err = open(f.name); err != nil {
			return nil, err
		}
	}
	w.polls.WriteString("arm,seq,op,sent_ms,recv_ms,n_tweets,n_new,err\n")
	w.rl.WriteString("ts_ms,endpoint,obs_limit,obs_remaining_min,obs_remaining_max,reset_at\n")
	w.wr.WriteString("ts_ms,arm,action,target,status,codes,err\n")
	return w, nil
}

func (w *writer) recordObservation(o observation, nNew int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	line, _ := json.Marshal(o)
	fmt.Fprintf(w.obs, "%s\n", line)
	errStr := o.Err
	if errStr == "" {
		errStr = ""
	}
	fmt.Fprintf(w.polls, "%s,%d,%s,%d,%d,%d,%d,%q\n",
		o.Arm, o.Seq, o.Op, o.Sent, o.Recv, len(o.Tweets), nNew, errStr)
}

func (w *writer) recordWrite(arm, action, target string, res xResult, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	fmt.Fprintf(w.wr, "%d,%s,%s,%s,%d,%v,%q\n",
		time.Now().UnixMilli(), arm, action, target, res.Status, res.Codes, errStr)
}

func (w *writer) recordRateLimit(endpoint string, limit, remMin, remMax int64, resetAt string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprintf(w.rl, "%d,%s,%d,%d,%d,%s\n",
		time.Now().UnixMilli(), endpoint, limit, remMin, remMax, resetAt)
}

func (w *writer) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, f := range []*os.File{w.obs, w.polls, w.rl, w.wr} {
		if f != nil {
			f.Close()
		}
	}
}

// --- OTel rate-limit sampler ------------------------------------------------

// rlSampler owns the ManualReader all three clients share and periodically
// drains the ratelimit histograms into ratelimit.csv.
type rlSampler struct {
	reader *metric.ManualReader
	w      *writer
	seen   map[string]*rlAcc
}

type rlAcc struct {
	limit  int64
	remMin int64
	remMax int64
	reset  time.Time
}

func newRLSampler(r *metric.ManualReader, w *writer) *rlSampler {
	return &rlSampler{reader: r, w: w, seen: map[string]*rlAcc{}}
}

// sampleOnce collects current metric state and appends rows for instruments
// whose values advanced since the last sample.
func (s *rlSampler) sampleOnce(ctx context.Context) {
	var rm metricdata.ResourceMetrics
	if err := s.reader.Collect(ctx, &rm); err != nil {
		return
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "twitter.client.ratelimit.limit" && m.Name != "twitter.client.ratelimit.remaining" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			if !ok {
				continue
			}
			for _, dp := range h.DataPoints {
				op := ""
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "twitter.operation" {
						op = kv.Value.AsString()
					}
				}
				if op == "" {
					continue
				}
				acc := s.seen[op]
				if acc == nil {
					acc = &rlAcc{remMin: -1}
					s.seen[op] = acc
				}
				maxV, _ := dp.Max.Value()
				minV, _ := dp.Min.Value()
				if m.Name == "twitter.client.ratelimit.limit" {
					if maxV > acc.limit {
						acc.limit = maxV
						s.w.recordRateLimit(op, maxV, -1, -1, "")
					}
				} else {
					if acc.remMin < 0 || minV < acc.remMin {
						acc.remMin = minV
					}
					if maxV > acc.remMax {
						acc.remMax = maxV
						s.w.recordRateLimit(op, acc.limit, acc.remMin, acc.remMax, "")
					}
				}
			}
		}
	}
}

func (s *rlSampler) run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.sampleOnce(context.Background())
			return
		case <-t.C:
			s.sampleOnce(ctx)
		}
	}
}

// observedLimit returns the largest x-rate-limit-limit observed so far for op.
func (s *rlSampler) observedLimit(op string) int64 {
	if a := s.seen[op]; a != nil {
		return a.limit
	}
	return 0
}

// --- materialized spec CSV ---------------------------------------------------

type tweetRow struct {
	ID        string
	Handle    string
	Type      string
	CreatedMs int64
	Arm       string
	Account   string
	LastMiss  int64
	FirstSeen int64
}

// dumpTweetsCSV rewrites tweets.csv atomically from the live tracker.
func dumpTweetsCSV(dir string, rows []*tweetRow) error {
	tmp := filepath.Join(dir, "tweets.csv.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	fmt.Fprintln(f, "tweet_id,handle,type,created_ms,arm,account,last_miss_ms,first_seen_ms")
	for _, r := range rows {
		fmt.Fprintf(f, "%s,%s,%s,%d,%s,%s,%d,%d\n",
			r.ID, r.Handle, r.Type, r.CreatedMs, r.Arm, r.Account, r.LastMiss, r.FirstSeen)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "tweets.csv"))
}
