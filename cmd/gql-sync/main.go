// Command gql-sync auto-extracts Twitter GraphQL queryIDs from x.com's webpack
// bundles and regenerates queryids_gen.go — the "generated" layer of the
// endpoints override chain (env > generated > committed).
//
// It builds the chunk map via internal/bundle (an HTTPFetcher that honors
// -proxy, or a disk fixtures fetcher for -fixtures), WalkImports over the
// bundles, extracts operationName -> queryId (deduped by source priority), and
// writes a gofmt'd, sorted, code-generated map. On an empty or failed
// extraction it leaves queryids_gen.go untouched so a transient x.com break can
// never clobber the committed IDs; -fail-on-empty additionally exits non-zero.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

const (
	warmHome          = "https://x.com/home"
	generatedFileName = "queryids_gen.go"
	// maxWalkBundles bounds the BFS over x.com's chunk graph. Set well above the
	// real top-level chunk count so a normal run never silently caps; WalkImports
	// logs bundle.bfs_capped if it ever hits this.
	maxWalkBundles                = 512
	generatedFilePerm os.FileMode = 0o644
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("gql-sync failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// config holds the parsed CLI flags.
type config struct {
	out         string
	proxy       string
	fixtures    string
	date        string
	failOnEmpty bool
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("gql-sync", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.out, "out", ".", "repo root to write queryids_gen.go")
	fs.StringVar(&c.proxy, "proxy", "", "proxy URL for the HTTPFetcher (ride Webshare from CI/datacenter)")
	fs.StringVar(&c.fixtures, "fixtures", "", "read warm pages/bundles from this dir instead of the network")
	fs.StringVar(&c.date, "date", "", "generation date stamp (e.g. 2026-06-19); omitted from output when empty")
	fs.BoolVar(&c.failOnEmpty, "fail-on-empty", false, "exit non-zero if 0 ops extracted (never overwrite on a break)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	return c, nil
}

func run(args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}

	fetcher, opts, err := buildFetcher(cfg)
	if err != nil {
		return err
	}

	ids, err := extractQueryIDs(context.Background(), fetcher, opts)
	if err != nil {
		return err
	}

	if len(ids) == 0 {
		if cfg.failOnEmpty {
			return fmt.Errorf("extracted 0 operations: refusing to overwrite %s", generatedFileName)
		}
		slog.Warn("gql-sync: extracted 0 operations; leaving " + generatedFileName + " untouched")
		return nil
	}

	return writeGenerated(cfg, ids)
}

// buildFetcher selects the disk (fixtures) or network (HTTP) Fetcher and the
// matching bundle.Options.
func buildFetcher(cfg config) (bundle.Fetcher, bundle.Options, error) {
	if cfg.fixtures != "" {
		opts := bundle.Options{WarmPages: []string{warmHome}, MaxBundles: maxWalkBundles}
		return &fileFetcher{dir: cfg.fixtures}, opts, nil
	}
	f, err := bundle.NewHTTPFetcher(cfg.proxy)
	if err != nil {
		return nil, bundle.Options{}, fmt.Errorf("build http fetcher: %w", err)
	}
	return f, bundle.Options{MaxBundles: maxWalkBundles}, nil
}

// extractQueryIDs builds the chunk map, walks every resolvable bundle, and runs
// the queryID extractor. It uses a throwaway cache dir so runs are independent
// and never serve a stale snapshot from a prior invocation.
func extractQueryIDs(ctx context.Context, fetcher bundle.Fetcher, opts bundle.Options) (map[string]string, error) {
	cacheDir, err := os.MkdirTemp("", "go-twitter-gqlsync-cache-")
	if err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(cacheDir) }()
	opts.CacheDir = cacheDir

	m, err := bundle.Build(ctx, fetcher, opts)
	if err != nil {
		return nil, fmt.Errorf("build chunk map: %w", err)
	}

	start := startBundles(m)
	ext := newExtractor(slog.Default())
	if err := m.WalkImports(ctx, fetcher, start, ext.consume, opts); err != nil {
		return nil, fmt.Errorf("walk bundles: %w", err)
	}

	ids := ext.result()
	slog.Info("gql-sync: extraction complete",
		slog.Int("bundles_started", len(start)), slog.Int("ops_found", len(ids)))
	return ids, nil
}

// startBundles resolves every module name in the chunk map to its bundle URL,
// sorted for a deterministic walk order.
func startBundles(m *bundle.Map) []string {
	urls := make([]string, 0, len(m.Names))
	for _, name := range m.Names {
		if u, ok := m.BundleURL(name); ok {
			urls = append(urls, u)
		}
	}
	sort.Strings(urls)
	return urls
}

// writeGenerated renders the map and writes it only when the bytes differ from
// the file already on disk, so an unchanged sync leaves the mtime alone.
func writeGenerated(cfg config, ids map[string]string) error {
	content, err := renderQueryIDs(ids, cfg.date)
	if err != nil {
		return err
	}
	target := filepath.Join(cfg.out, generatedFileName)

	if existing, readErr := os.ReadFile(target); readErr == nil && bytes.Equal(existing, content) {
		slog.Info("gql-sync: "+generatedFileName+" already up to date; no rewrite",
			slog.String("path", target), slog.Int("ops", len(ids)))
		return nil
	}
	if err := os.WriteFile(target, content, generatedFilePerm); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	slog.Info("gql-sync: wrote "+generatedFileName,
		slog.String("path", target), slog.Int("ops", len(ids)))
	return nil
}
