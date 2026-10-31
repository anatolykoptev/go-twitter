// Command xweb-report crawls x.com's PUBLIC logged-out web shell, parses the
// x-web (Vite + Relay) operation artifacts out of its chunk graph and writes a
// markdown comparison against every GraphQL operation go-twitter issues
// (docs/xweb-migration.md). It sends no cookies and makes no API call: only
// static JavaScript from x.com and abs.twimg.com is read.
//
//	go run ./cmd/xweb-report -cache ~/tmp/xweb-cache -out docs/xweb-migration.md
//	go run ./cmd/xweb-report -cache testdata/site -offline      # no network
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	twitter "github.com/anatolykoptev/go-twitter"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/internal/gqlextract"
)

const shellURL = "https://x.com/home"

type config struct {
	out, cache, proxy, date string
	offline                 bool
	gap                     time.Duration
	concurrency             int
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("xweb-report", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.out, "out", "", "write the markdown report here (default: stdout)")
	fs.StringVar(&c.cache, "cache", "", "directory caching every fetched page/chunk (re-runs and tests read it instead of the network)")
	fs.StringVar(&c.proxy, "proxy", "", "proxy URL for the HTTP fetcher")
	fs.StringVar(&c.date, "date", "", "crawl date stamp for the report header (omitted when empty)")
	fs.BoolVar(&c.offline, "offline", false, "never touch the network; a cache miss is a failure")
	fs.DurationVar(&c.gap, "gap", 100*time.Millisecond, "pause each worker takes between two requests")
	fs.IntVar(&c.concurrency, "concurrency", bundle.DefaultCrawlConcurrency, "parallel chunk fetches")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if c.offline && c.cache == "" {
		return config{}, errors.New("-offline needs -cache")
	}
	return c, nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("xweb-report failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}
	// The committed ids in the report must be the compiled ones, not whatever an
	// operator's environment pins.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TWITTER_QID_") {
			return fmt.Errorf("refusing to run with %s set: it would change the committed ids in the report",
				strings.SplitN(kv, "=", 2)[0])
		}
	}
	var inner bundle.Fetcher
	if !cfg.offline {
		h, err := bundle.NewHTTPFetcher(cfg.proxy)
		if err != nil {
			return fmt.Errorf("build http fetcher: %w", err)
		}
		inner = h // no Cookie: this tool reads public assets only
	}
	var f bundle.Fetcher = inner
	if cfg.cache != "" {
		f = &cacheFetcher{dir: cfg.cache, inner: inner, offline: cfg.offline}
	}
	md, err := generate(context.Background(), f, cfg)
	if err != nil {
		return err
	}
	if cfg.out == "" {
		_, err = os.Stdout.WriteString(md)
		return err
	}
	return os.WriteFile(cfg.out, []byte(md), 0o644)
}

// generate runs the crawl and renders the report.
func generate(ctx context.Context, f bundle.Fetcher, cfg config) (string, error) {
	page, err := bundle.FetchPage(ctx, f, shellURL)
	if err != nil {
		return "", fmt.Errorf("fetch shell: %w", err)
	}
	scripts := bundle.ViteScripts(string(page), shellURL)
	if len(scripts) == 0 {
		return "", errors.New("the shell references no x-web script (layout changed, or a bot wall)")
	}
	ext := gqlextract.NewXWebExtractor(slog.Default())
	st, err := bundle.CrawlXWeb(ctx, f, scripts, ext.Consume,
		bundle.CrawlOptions{Concurrency: cfg.concurrency, Gap: cfg.gap})
	if err != nil {
		return "", err
	}
	ops := ext.Ops()
	if len(ops) == 0 {
		return "", fmt.Errorf("crawl parsed no Relay artifact (%d chunks fetched): refusing to write an empty report", st.Fetched)
	}
	ids := make(map[string]string, len(twitter.Endpoints))
	for name, ep := range twitter.Endpoints {
		ids[name] = ep.ID
	}
	m := meta{Entry: baseName(scripts[0]), Fetched: st.Fetched, Failed: st.Failed, Capped: st.Capped,
		Artifacts: len(ops), Conflicts: ext.Conflicts(), GeneratedFor: cfg.date}
	for _, o := range ops {
		if o.Kind == "mutation" {
			m.Mutations++
		} else {
			m.Queries++
		}
	}
	return render(build(ids, ops), m), nil
}

func baseName(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}
