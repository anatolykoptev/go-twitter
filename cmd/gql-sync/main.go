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
	featuresFileName  = "features_gen.go"
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

	ctx := context.Background()
	ids, featureNames, err := extractAll(ctx, fetcher, opts)
	if err != nil {
		return err
	}

	// queryIDs are written first and independently: a failure fetching/parsing the
	// feature defaults must never block a good queryID refresh.
	if err := syncQueryIDs(cfg, ids); err != nil {
		return err
	}

	// Feature defaults live in the warm page's featureSwitch.defaultConfig, not in
	// any JS chunk — fetch the home page once and parse them. A defaults-fetch
	// failure degrades gracefully (queryIDs already written): warn and leave
	// features_gen.go untouched unless -fail-on-empty makes it fatal.
	defaults, err := fetchFeatureDefaults(ctx, fetcher)
	if err != nil {
		if cfg.failOnEmpty {
			return err
		}
		slog.Warn("gql-sync: feature-defaults fetch failed; leaving "+featuresFileName+" untouched",
			slog.Any("error", err))
		return nil
	}
	return syncFeatures(cfg, featureNames, defaults)
}

// syncQueryIDs writes queryids_gen.go, honoring the empty-extraction guard so a
// transient x.com break never clobbers the committed IDs. The write is ADDITIVE:
// endpoints already present in the on-disk generatedQueryIDs but NOT re-extracted
// by this run (e.g. session-only ops gql-sync cannot reach from the public
// bundles) are preserved — gql-sync merges over the committed baseline instead
// of replacing it wholesale. Without this, every drift run would drop the
// session-only IDs and fail TestQueryIDCompleteness (issue #39).
func syncQueryIDs(cfg config, ids map[string]string) error {
	if len(ids) == 0 {
		if cfg.failOnEmpty {
			return fmt.Errorf("extracted 0 operations: refusing to overwrite %s", generatedFileName)
		}
		slog.Warn("gql-sync: extracted 0 operations; leaving " + generatedFileName + " untouched")
		return nil
	}
	merged := mergeCommittedQueryIDs(cfg, ids)
	return writeGenerated(cfg, merged)
}

// mergeCommittedQueryIDs reads the on-disk queryids_gen.go and preserves any
// committed entry that this run did NOT re-extract. Newly extracted IDs override
// committed ones (x.com rotated the queryID); committed entries absent from the
// extraction are kept verbatim (session-only ops gql-sync cannot reach).
func mergeCommittedQueryIDs(cfg config, extracted map[string]string) map[string]string {
	target := filepath.Join(cfg.out, generatedFileName)
	body, err := os.ReadFile(target)
	if err != nil {
		// No prior file — nothing to merge, write the extraction as-is.
		return extracted
	}
	committed := parseGeneratedQueryIDs(body)
	if len(committed) == 0 {
		return extracted
	}
	merged := make(map[string]string, len(extracted)+len(committed))
	for name, id := range committed {
		merged[name] = id
	}
	preserved := 0
	for name, id := range extracted {
		if _, ok := merged[name]; !ok {
			merged[name] = id
		} else if merged[name] != id {
			// x.com rotated the queryID — override with the fresh extraction.
			merged[name] = id
		}
	}
	// Count preserved for logging: committed entries NOT in the extraction.
	for name := range committed {
		if _, ok := extracted[name]; !ok {
			preserved++
		}
	}
	if preserved > 0 {
		slog.Info("gql-sync: preserved session-only queryIDs from committed baseline",
			slog.Int("preserved", preserved),
			slog.Int("extracted", len(extracted)),
			slog.Int("merged_total", len(merged)))
	}
	return merged
}

// syncFeatures writes features_gen.go from the bundle-declared feature NAMES
// intersected with the warm-page DEFAULTS. Like the queryID pass it refuses to
// overwrite on an empty extraction (a transient break never blanks the baseline);
// -fail-on-empty makes that fatal. Baseline flags that vanished from the bundle
// are surfaced as a // REMOVED comment, never silently dropped.
func syncFeatures(cfg config, names []string, defaults map[string]bool) error {
	features, _ := intersectFeatures(names, defaults, slog.Default())
	if len(features) == 0 {
		if cfg.failOnEmpty {
			return fmt.Errorf("extracted 0 features: refusing to overwrite %s", featuresFileName)
		}
		slog.Warn("gql-sync: extracted 0 features; leaving " + featuresFileName + " untouched")
		return nil
	}
	return writeFeatures(cfg, features)
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

// extractAll builds the chunk map and walks every resolvable bundle ONCE,
// fanning each body into both the queryID extractor and the feature-name
// collector so a single invocation refreshes both generated files. It uses a
// throwaway cache dir so runs are independent and never serve a stale snapshot.
func extractAll(ctx context.Context, fetcher bundle.Fetcher, opts bundle.Options) (map[string]string, []string, error) {
	cacheDir, err := os.MkdirTemp("", "go-twitter-gqlsync-cache-")
	if err != nil {
		return nil, nil, fmt.Errorf("create cache dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(cacheDir) }()
	opts.CacheDir = cacheDir

	m, err := bundle.Build(ctx, fetcher, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("build chunk map: %w", err)
	}

	start := startBundles(m)
	ext := newExtractor(slog.Default())
	feat := newFeatureCollector()
	visit := func(url string, body []byte) {
		ext.consume(url, body)
		feat.consume(url, body)
	}
	if err := m.WalkImports(ctx, fetcher, start, visit, opts); err != nil {
		return nil, nil, fmt.Errorf("walk bundles: %w", err)
	}

	ids := ext.result()
	names := feat.result()
	slog.Info("gql-sync: extraction complete",
		slog.Int("bundles_started", len(start)),
		slog.Int("ops_found", len(ids)),
		slog.Int("feature_names_found", len(names)))
	return ids, names, nil
}

// fetchFeatureDefaults fetches the warm home page and parses the feature-switch
// defaults from its featureSwitch.defaultConfig. The defaults are NOT in the JS
// bundles, so this is a separate fetch from the chunk-map walk.
func fetchFeatureDefaults(ctx context.Context, fetcher bundle.Fetcher) (map[string]bool, error) {
	body, err := fetcher.Fetch(ctx, warmHome)
	if err != nil {
		return nil, fmt.Errorf("fetch warm page for feature defaults: %w", err)
	}
	defaults := parseFeatureDefaults(body)
	slog.Info("gql-sync: parsed feature defaults", slog.Int("defaults_found", len(defaults)))
	return defaults, nil
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

// writeFeatures renders features_gen.go from the extracted feature map, diffing
// against the baseline already on disk so a baseline flag that vanished surfaces
// as a // REMOVED comment. Like writeGenerated it only writes when the bytes
// differ, leaving an unchanged file's mtime alone.
func writeFeatures(cfg config, features map[string]any) error {
	target := filepath.Join(cfg.out, featuresFileName)

	// The on-disk baseline (the prior features_gen.go, == committedFeatures()
	// verbatim) is the COMMITTED-VALUE authority: known flags keep these values,
	// the warm-page guest defaults are emitted only as comments. A vanished
	// baseline flag surfaces as // REMOVED.
	var baseline map[string]bool
	if existing, readErr := os.ReadFile(target); readErr == nil {
		baseline = parseFeatureBaseline(existing)
	}
	removed := removedFeatures(features, baseline)

	content, err := renderFeatures(features, baseline, removed, cfg.date)
	if err != nil {
		return err
	}

	if existing, readErr := os.ReadFile(target); readErr == nil && bytes.Equal(existing, content) {
		slog.Info("gql-sync: "+featuresFileName+" already up to date; no rewrite",
			slog.String("path", target), slog.Int("features", len(features)))
		return nil
	}
	if err := os.WriteFile(target, content, generatedFilePerm); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	slog.Info("gql-sync: wrote "+featuresFileName,
		slog.String("path", target), slog.Int("features", len(features)), slog.Int("removed", len(removed)))
	return nil
}
