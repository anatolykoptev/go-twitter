package bundle

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

// cdnHost serves the content-hashed, immutable JS chunks. Only these are safe to
// share between consumers: x.com pages are per-account and never cached.
const cdnHost = "abs.twimg.com"

// MemoCache is a short-lived, size-bounded body cache for immutable CDN chunks.
// It lets two consumers of one authenticated session (xtid's indices scan and
// the queryId refresh) read each chunk from X once. It is safe for concurrent
// use; when full it stops admitting new bodies rather than evicting live ones.
type MemoCache struct {
	ttl      time.Duration
	maxBytes int
	now      func() time.Time

	mu    sync.Mutex
	items map[string]memoItem
	bytes int
}

type memoItem struct {
	body []byte
	at   time.Time
}

// NewMemoCache returns a cache holding bodies for ttl, at most maxBytes in
// total. now == nil uses time.Now.
func NewMemoCache(ttl time.Duration, maxBytes int, now func() time.Time) *MemoCache {
	if now == nil {
		now = time.Now
	}
	return &MemoCache{ttl: ttl, maxBytes: maxBytes, now: now, items: make(map[string]memoItem)}
}

func (c *MemoCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if c.now().Sub(it.at) > c.ttl {
		c.bytes -= len(it.body)
		delete(c.items, key)
		return nil, false
	}
	return it.body, true
}

func (c *MemoCache) put(key string, body []byte) {
	if len(body) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, it := range c.items { // purge expired entries before judging fullness
		if now.Sub(it.at) > c.ttl {
			c.bytes -= len(it.body)
			delete(c.items, k)
		}
	}
	if old, ok := c.items[key]; ok {
		c.bytes -= len(old.body)
		delete(c.items, key)
	}
	if c.bytes+len(body) > c.maxBytes {
		return
	}
	c.items[key] = memoItem{body: body, at: now}
	c.bytes += len(body)
}

// MemoFetcher serves immutable CDN chunk URLs through Cache and passes every
// other URL (x.com pages, anything per-account) straight to Inner.
type MemoFetcher struct {
	Inner Fetcher
	Cache *MemoCache
}

// Fetch implements Fetcher. Cached bodies are shared, not copied: callers must
// not mutate them (Fetcher consumers here only read).
func (f *MemoFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	if !cacheableChunk(rawURL) {
		return f.Inner.Fetch(ctx, rawURL)
	}
	if body, ok := f.Cache.get(rawURL); ok {
		return body, nil
	}
	body, err := f.Inner.Fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	f.Cache.put(rawURL, body)
	return body, nil
}

// PostForm forwards to Inner when it can POST (the x.com/x/migrate hop of
// FetchPage), so wrapping never hides that capability.
func (f *MemoFetcher) PostForm(ctx context.Context, rawURL string, form url.Values) ([]byte, error) {
	p, ok := f.Inner.(Poster)
	if !ok {
		return nil, errors.New("bundle: fetcher cannot POST")
	}
	return p.PostForm(ctx, rawURL, form)
}

func cacheableChunk(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && u.Scheme == "https" && u.Hostname() == cdnHost && strings.HasSuffix(u.Path, ".js")
}
