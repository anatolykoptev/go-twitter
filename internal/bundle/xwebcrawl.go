package bundle

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// x-web chunk graph. The Vite/rolldown build links chunks three ways, all as
// plain string literals:
//
//	import{…}from"./assets/x-HASH.js"      static import (relative to the importer)
//	import("./x-HASH.js")                   dynamic import (relative to the importer)
//	["assets/x-HASH.js", …]                 __vite__mapDeps preload list (relative
//	                                        to the entry script's directory)
//
// WalkImports follows only the dynamic form, which reaches a fraction of the
// graph; CrawlXWeb follows all three.
var xwebRefRe = regexp.MustCompile("[\"'`]((?:\\./)?(?:assets/)?[\\w@.\\-/]+\\.js)[\"'`]")

const (
	// DefaultCrawlMax bounds the fetches of one crawl. The logged-out shell's
	// graph measured ~2000 chunks (2026-10-30); the cap leaves headroom without
	// letting a hostile graph run unbounded.
	DefaultCrawlMax = 3000
	// DefaultCrawlConcurrency is deliberately low: these are public CDN assets
	// but a crawl is thousands of requests.
	DefaultCrawlConcurrency = 4
)

// CrawlOptions bounds CrawlXWeb. Zero values take the defaults.
type CrawlOptions struct {
	MaxFetch    int
	Concurrency int
	// Gap is the minimum pause a worker takes between two of its fetches.
	Gap time.Duration
	// Done, when non-nil, is polled between small batches; returning true ends
	// the crawl early (the runtime refresher stops once it has the ids it wants).
	Done func() bool
}

// CrawlStats reports what a crawl did.
type CrawlStats struct {
	Fetched int
	Failed  int
	Capped  bool // MaxFetch reached with chunks still queued
	Stopped bool // Done ended the crawl
}

// CrawlXWeb walks the x-web chunk graph breadth-first from the start scripts,
// calling visit (serially, from the calling goroutine) with each fetched body.
// Only https + allowlisted hosts are fetched. Per-chunk failures are counted
// and skipped, never fatal (several string literals that look like chunk paths
// are not chunks).
func CrawlXWeb(ctx context.Context, f Fetcher, start []string,
	visit func(chunkURL string, body []byte), opts CrawlOptions) (CrawlStats, error) {
	if opts.MaxFetch <= 0 {
		opts.MaxFetch = DefaultCrawlMax
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultCrawlConcurrency
	}
	roots := map[string]string{} // chunk URL -> directory "assets/…" literals resolve against
	seen := map[string]bool{}
	var queue []string
	for _, s := range start {
		if seen[s] || !safePageURL(s) {
			continue
		}
		seen[s] = true
		queue = append(queue, s)
		roots[s] = dirOf(s)
	}

	var st CrawlStats
	batchSize := opts.Concurrency * 4
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		if st.Fetched+st.Failed >= opts.MaxFetch {
			st.Capped = true
			slog.Warn("bundle.xweb_crawl_capped",
				slog.Int("max_fetch", opts.MaxFetch), slog.Int("queued", len(queue)))
			return st, nil
		}
		n := min(batchSize, len(queue), opts.MaxFetch-st.Fetched-st.Failed)
		batch := queue[:n]
		queue = queue[n:]
		bodies := fetchBatch(ctx, f, batch, opts)
		for i, body := range bodies {
			if body == nil {
				st.Failed++
				continue
			}
			st.Fetched++
			visit(batch[i], body)
			queue = enqueueXWebRefs(queue, seen, roots, batch[i], body)
		}
		if opts.Done != nil && opts.Done() {
			st.Stopped = true
			return st, nil
		}
	}
	return st, nil
}

// fetchBatch fetches urls with up to opts.Concurrency workers; a failed fetch
// yields a nil entry.
func fetchBatch(ctx context.Context, f Fetcher, urls []string, opts CrawlOptions) [][]byte {
	out := make([][]byte, len(urls))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(opts.Concurrency, len(urls)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				body, err := f.Fetch(ctx, urls[i])
				if err != nil {
					slog.Debug("bundle: xweb chunk fetch failed", slog.String("url", urls[i]), slog.Any("error", err))
				} else {
					out[i] = body
				}
				if opts.Gap > 0 {
					select {
					case <-time.After(opts.Gap):
					case <-ctx.Done():
					}
				}
			}
		}()
	}
	for i := range urls {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

// enqueueXWebRefs appends every unseen chunk reference in body to queue. A
// child inherits its parent's root directory, so "assets/…" literals resolve
// against the entry script's directory even from deep chunks.
func enqueueXWebRefs(queue []string, seen map[string]bool, roots map[string]string, parent string, body []byte) []string {
	for _, m := range xwebRefRe.FindAllSubmatch(body, -1) {
		child, ok := resolveXWebRef(parent, roots[parent], string(m[1]))
		if !ok || seen[child] {
			continue
		}
		seen[child] = true
		roots[child] = roots[parent]
		queue = append(queue, child)
	}
	return queue
}

// resolveXWebRef resolves a chunk literal: "./x.js" against the importer's URL,
// "assets/x.js" against the entry directory. Anything else (a bare module name,
// a ".." escape, another host) is not a chunk reference.
func resolveXWebRef(parent, root, ref string) (string, bool) {
	var base string
	switch {
	case len(ref) > 2 && ref[:2] == "./":
		base = parent
	case len(ref) > 7 && ref[:7] == "assets/":
		base = root
	default:
		return "", false
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", false
	}
	abs := b.ResolveReference(r)
	if abs.RawQuery != "" || abs.Fragment != "" || !safePageURL(abs.String()) {
		return "", false
	}
	return abs.String(), true
}

// dirOf returns the directory URL (with trailing slash) of a script URL.
func dirOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = u.Path[:len(u.Path)-len(pathBase(u.Path))]
	return u.String()
}

func pathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
