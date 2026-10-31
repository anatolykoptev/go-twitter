package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

// cacheFetcher serves a page or chunk from a local tree when present and
// otherwise (unless offline) from the network, storing what it fetched. The
// tree mirrors URL paths (host dropped), with the extension-less shell page
// stored as <name>.html, so a cache directory doubles as a test fixture.
type cacheFetcher struct {
	dir     string
	inner   bundle.Fetcher
	offline bool
}

var errOfflineMiss = errors.New("not in cache and -offline is set")

func (c *cacheFetcher) Fetch(ctx context.Context, raw string) ([]byte, error) {
	rel, err := cachePath(raw)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(c.dir, rel)
	if b, err := os.ReadFile(p); err == nil {
		return b, nil
	}
	if c.offline || c.inner == nil {
		return nil, fmt.Errorf("%s: %w", raw, errOfflineMiss)
	}
	b, err := c.inner.Fetch(ctx, raw)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		_ = os.WriteFile(p, b, 0o644)
	}
	return b, nil
}

// cachePath maps a URL to its relative cache file.
func cachePath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	p := strings.TrimPrefix(filepath.Clean("/"+u.Path), "/")
	if p == "" {
		p = "index"
	}
	if !strings.HasSuffix(p, ".js") {
		p += ".html"
	}
	return p, nil
}
