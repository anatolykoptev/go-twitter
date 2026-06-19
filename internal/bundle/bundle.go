package bundle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultCacheTTL   = 6 * time.Hour
	defaultMaxBundles = 64
)

// Sentinel errors let consumers (xtid / gql-sync) route failures to the right
// PoolAlertHook topic without string matching.
var (
	ErrFetchFailed   = errors.New("bundle: warm-page fetch failed")
	ErrEmptyChunkMap = errors.New("bundle: chunk-map reassembly empty")
)

// Options drives Build and bounds WalkImports. Zero-value fields take the
// documented defaults.
type Options struct {
	WarmPages  []string      // default: x.com/home, x.com/xdevelopers
	CacheDir   string        // default: <tmp>/go-twitter-bundle
	CacheTTL   time.Duration // default: 6h
	MaxBundles int           // default: 64 — BFS cap
}

func defaultWarmPages() []string {
	return []string{"https://x.com/home", "https://x.com/xdevelopers"}
}

func (o *Options) applyDefaults() {
	if len(o.WarmPages) == 0 {
		o.WarmPages = defaultWarmPages()
	}
	if o.CacheDir == "" {
		o.CacheDir = filepath.Join(os.TempDir(), "go-twitter-bundle")
	}
	if o.CacheTTL == 0 {
		o.CacheTTL = defaultCacheTTL
	}
	if o.MaxBundles == 0 {
		o.MaxBundles = defaultMaxBundles
	}
}

// Build fetches the warm pages with f, reassembles the webpack chunk map, and
// caches it on disk. A fresh on-disk cache short-circuits the network. On fetch
// or reassembly failure it falls back to a stale cache when one exists,
// otherwise returns a wrapped ErrFetchFailed / ErrEmptyChunkMap so the caller
// can keep its committed-literal values.
func Build(ctx context.Context, f Fetcher, opts Options) (*Map, error) {
	opts.applyDefaults()
	cache := &diskCache{dir: opts.CacheDir, ttl: opts.CacheTTL}

	if m, ok := cache.load(time.Now()); ok {
		return m, nil
	}

	m, err := fetchAndReassemble(ctx, f, opts)
	if err != nil {
		if stale, ok := cache.loadStale(); ok {
			slog.Warn("bundle: using stale cache after fetch failure", slog.Any("error", err))
			return stale, nil
		}
		return nil, err
	}

	if storeErr := cache.store(m); storeErr != nil {
		slog.Warn("bundle: cache store failed", slog.Any("error", storeErr))
	}
	return m, nil
}

// fetchAndReassemble pulls every warm page and merges their chunk maps. It
// returns an error (not a partial map) when either dict ends up empty.
func fetchAndReassemble(ctx context.Context, f Fetcher, opts Options) (*Map, error) {
	names := make(map[string]string)
	hashes := make(map[string]string)
	fetched := 0
	var lastFetchErr error

	for _, page := range opts.WarmPages {
		body, err := f.Fetch(ctx, page)
		if err != nil {
			lastFetchErr = err
			slog.Warn("bundle: warm-page fetch failed",
				slog.String("url", page), slog.Any("error", err))
			continue
		}
		fetched++
		n, h := parseChunkMap(string(body))
		mergeInto(names, n)
		mergeInto(hashes, h)
	}

	if len(names) == 0 || len(hashes) == 0 {
		return nil, reassembleError(fetched, len(names), len(hashes), lastFetchErr)
	}
	return &Map{Names: names, Hashes: hashes, FetchedAt: time.Now()}, nil
}

func reassembleError(fetched, names, hashes int, fetchErr error) error {
	if fetched == 0 && fetchErr != nil {
		return fmt.Errorf("%w: %w", ErrFetchFailed, fetchErr)
	}
	return fmt.Errorf("%w: names=%d hashes=%d", ErrEmptyChunkMap, names, hashes)
}
