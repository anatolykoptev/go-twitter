package bundle

import (
	"context"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

type countFetcher struct {
	hits atomic.Int32
	body string
}

func (f *countFetcher) Fetch(context.Context, string) ([]byte, error) {
	f.hits.Add(1)
	return []byte(f.body), nil
}

func (f *countFetcher) PostForm(context.Context, string, url.Values) ([]byte, error) {
	return []byte("posted"), nil
}

func TestMemoFetcher_ChunkFetchedOnce_PagesNever(t *testing.T) {
	inner := &countFetcher{body: "chunk"}
	clock := time.Unix(1_000_000, 0)
	mf := &MemoFetcher{Inner: inner, Cache: NewMemoCache(time.Minute, 1<<20, func() time.Time { return clock })}
	const chunk = "https://abs.twimg.com/x-web/entry-0000.js"
	const page = "https://x.com/tesla"

	for i := 0; i < 3; i++ {
		if _, err := mf.Fetch(context.Background(), chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := mf.Fetch(context.Background(), page); err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.hits.Load(); got != 1+3 {
		t.Fatalf("inner fetches = %d, want 1 (chunk, cached) + 3 (page, never cached)", got)
	}

	clock = clock.Add(2 * time.Minute) // past the ttl
	if _, err := mf.Fetch(context.Background(), chunk); err != nil {
		t.Fatal(err)
	}
	if got := inner.hits.Load(); got != 1+3+1 {
		t.Fatalf("expired chunk not refetched: inner fetches = %d", got)
	}
}

func TestMemoCache_SizeBound(t *testing.T) {
	c := NewMemoCache(time.Minute, 10, nil)
	c.put("a", []byte("123456"))
	c.put("b", []byte("123456")) // would exceed 10 bytes: not admitted
	if _, ok := c.get("a"); !ok {
		t.Error("live entry a was evicted")
	}
	if _, ok := c.get("b"); ok {
		t.Error("entry b admitted past the byte bound")
	}
	c.put("huge", make([]byte, 11))
	if _, ok := c.get("huge"); ok {
		t.Error("oversized body admitted")
	}
}

func TestMemoFetcher_KeepsPosterCapability(t *testing.T) {
	mf := &MemoFetcher{Inner: &countFetcher{}, Cache: NewMemoCache(time.Minute, 1, nil)}
	var _ Poster = mf
	body, err := mf.PostForm(context.Background(), "https://x.com/x/migrate", nil)
	if err != nil || string(body) != "posted" {
		t.Fatalf("PostForm = %q, %v", body, err)
	}
}
