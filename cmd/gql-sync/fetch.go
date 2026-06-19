package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
)

// fileFetcher serves warm pages and bundles from a local fixtures directory,
// implementing bundle.Fetcher for the -fixtures (offline / CI) mode. A URL is
// resolved to a file by its basename: warm pages (no extension) get ".html"
// appended; bundle URLs already end in ".js" and map verbatim.
type fileFetcher struct{ dir string }

func (f *fileFetcher) Fetch(_ context.Context, rawURL string) ([]byte, error) {
	name, err := fixtureFileName(rawURL)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		return nil, fmt.Errorf("fixture fetch %s: %w", rawURL, err)
	}
	return data, nil
}

// fixtureFileName maps a URL to its on-disk fixture filename.
func fixtureFileName(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url %s: %w", rawURL, err)
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		return "", fmt.Errorf("no fixture name in url %s", rawURL)
	}
	if path.Ext(base) == "" {
		base += ".html" // warm page (e.g. /home) -> home.html
	}
	return base, nil
}
