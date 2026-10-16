package twitter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/xtid"
)

// Webpack-layout refresh (#68 follow-up). Live, the authenticated x.com page is
// still the legacy responsive-web/client-web build: no x-web script links, a
// chunk map inline, ids in main.<hash>a.js and endpoint chunks. xtid reaches
// ondemand.s through that layout (xtid.locateIndices), so the refresh must read
// the same bundles.

const (
	wpBase      = "https://abs.twimg.com/responsive-web/client-web/"
	wpMainURL   = wpBase + "main.1111111a.js"
	wpEndptURL  = wpBase + "api.endpoints.2222222a.js"
	wpOnDemand  = wpBase + "ondemand.s.3333333a.js"
	wpEvilURL   = "https://evil.example.com/responsive-web/client-web/main.9999999a.js"
	wpRetireOps = 8
)

// qidWebpackWorld is the fake X serving the legacy webpack-layout page. The ops
// are split across main and the endpoint chunk by index parity.
type qidWebpackWorld struct {
	*qidWorld
	extraScripts string // appended to <head>
	fetchedMu    sync.Mutex
	fetched      []string
}

func (w *qidWebpackWorld) page() []byte {
	base := string(w.qidWorld.pageHTML())
	// Replace the Vite entry link with the webpack markup: a main script tag, the
	// inline chunk map (ids -> names, ids -> hashes) and the legacy ondemand.s embed.
	base = strings.Replace(base, `<script type="module" src="`+qidEntryURL+`"></script>`,
		`<script src="`+wpMainURL+`"></script>`+w.extraScripts+
			`<script>window.__CHUNKS__={101:"main",102:"api.endpoints"};`+
			`window.__HASHES__={101:"1111111",102:"2222222"};`+
			`window.__LEGACY__={"ondemand.s":"3333333"};</script>`, 1)
	return []byte(base)
}

func (w *qidWebpackWorld) opsJS(parity int) []byte {
	ops := make([]string, 0, len(w.advertised))
	for op := range w.advertised {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	var b strings.Builder
	for i, op := range ops {
		if i%2 == parity {
			fmt.Fprintf(&b, `{queryId:%q,operationName:%q,operationType:"query",metadata:{}},`, w.advertised[op], op)
		}
	}
	return []byte(`"use strict";var ops=[` + b.String() + `];`)
}

func (w *qidWebpackWorld) DoCtx(_ context.Context, _, u string, _ map[string]string, _ io.Reader) ([]byte, map[string]string, int, error) {
	w.fetchedMu.Lock()
	w.fetched = append(w.fetched, u)
	w.fetchedMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hits[u]++
	switch u {
	case qidWarmPage:
		return w.page(), nil, http.StatusOK, nil
	case wpMainURL:
		return w.opsJS(0), nil, http.StatusOK, nil
	case wpEndptURL:
		return w.opsJS(1), nil, http.StatusOK, nil
	case wpOnDemand:
		return []byte(testIndicesJS), nil, http.StatusOK, nil
	}
	return nil, nil, http.StatusNotFound, nil
}

func (w *qidWebpackWorld) wasFetched(u string) bool {
	w.fetchedMu.Lock()
	defer w.fetchedMu.Unlock()
	for _, f := range w.fetched {
		if f == u {
			return true
		}
	}
	return false
}

func newQIDWebpackClient(t *testing.T) (*Client, *qidWebpackWorld) {
	t.Helper()
	w := &qidWebpackWorld{qidWorld: newQIDWorld(t)}
	w.start(t)
	c, _ := newPoolWaitClient(t, 1)
	c.warmDoerOverride = w
	c.xtidMgr.SetWarmPageHook(observeWarmPage)
	return c, w
}

// F1 (webpack) — journey: the authenticated warm page is the legacy webpack
// layout. main.<hash>a.js and the endpoint chunk carry new ids for 8 known ops.
// The first-trigger refresh must read them, and the next SearchTimeline call must
// use the new id.
//
// Mutation: in queryid_refresh.go parseWarmPage change
// `scripts: append(vite, web...)` to `scripts: vite` => RED ("the webpack page
// must yield a snapshot": the refresh fails with no scripts).
func TestQIDRefresh_WebpackF1_AuthedWebpackPageRefreshes(t *testing.T) {
	r, _ := useQIDRefresher(t)
	c, w := newQIDWebpackClient(t)
	ctx := ctxTimeout(t)

	ops := []string{"SearchTimeline"}
	rest := make([]string, 0, len(Endpoints))
	for op := range Endpoints {
		if op != "SearchTimeline" {
			rest = append(rest, op)
		}
	}
	sort.Strings(rest)
	ops = append(ops, rest[:wpRetireOps-1]...)
	for i, op := range ops {
		w.retire(op, fmt.Sprintf("WpNewID_%02d_abc", i))
	}
	w.mu.Lock()
	newSearch := w.live["SearchTimeline"]
	w.mu.Unlock()
	require.NotEqual(t, Endpoints["SearchTimeline"].ID, newSearch, "SearchTimeline must be among the retired ops")

	_ = c.search(ctx) // 404 on the compiled id; the first-trigger refresh reads the webpack bundles
	r.wg.Wait()

	snap := activeQID.Load()
	require.NotNil(t, snap, "the webpack page must yield a snapshot")
	assert.GreaterOrEqual(t, snap.changed, wpRetireOps)
	assert.True(t, w.wasFetched(wpMainURL), "main.<hash>a.js must be walked")
	assert.True(t, w.wasFetched(wpEndptURL), "the endpoint chunk from the chunk map must be walked")

	require.NoError(t, c.search(ctx), "the next call must use the refreshed id")
	ids := w.requestedIDs()
	assert.Equal(t, newSearch, ids[len(ids)-1])
}

// F3 — the zero-scripts error names the layout and the page size, so the next
// live failure explains itself.
//
// Mutation: in queryid_refresh.go build revert the message to
// "queryid refresh: no x-web scripts on the warm page" => RED.
func TestQIDRefresh_WebpackF3_ZeroScriptsErrorNamesLayoutAndSize(t *testing.T) {
	r, _ := useQIDRefresher(t)
	page := []byte(`<html><body>login wall, no scripts at all</body></html>`)
	f := &recordingFetcher{pages: map[string][]byte{xtid.AuthedWarmPageURL: page}}

	var seen pageSeen
	_, err := r.build(context.Background(), f, &seen)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "layout=unknown")
	assert.Contains(t, err.Error(), fmt.Sprintf("page_bytes=%d", len(page)))
	assert.Equal(t, qidLayoutUnknown, seen.layout)
	assert.Zero(t, seen.scripts)
}

// parseWarmPage reports the detected layout and the start-script count that the
// "queryid refresh finished" line logs.
func TestQIDRefresh_ParseWarmPageLayouts(t *testing.T) {
	vite := `<script type="module" src="` + qidEntryURL + `"></script>`
	web := `<script src="` + wpMainURL + `"></script>`
	for _, tc := range []struct {
		name, page, layout string
		scripts            int
	}{
		{"vite", vite, qidLayoutVite, 1},
		{"webpack", web, qidLayoutWebpack, 1},
		{"both", vite + web, qidLayoutBoth, 2},
		{"unknown", `<html></html>`, qidLayoutUnknown, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := parseWarmPage([]byte(tc.page), time.Now())
			assert.Equal(t, tc.layout, p.layout)
			assert.Len(t, p.scripts, tc.scripts)
		})
	}
}

// F4 — a webpack script on a non-allowlisted host is never fetched.
//
// Mutation: in internal/bundle/webpack.go WebpackScripts delete the
// `u.Scheme != "https" || !allowedBundleHost(u.Hostname())` check => RED (the
// evil URL is fetched).
func TestQIDRefresh_WebpackF4_NonAllowlistedHostIsNotFetched(t *testing.T) {
	r, _ := useQIDRefresher(t)
	page := `<script src="` + wpEvilURL + `"></script><script src="` + wpMainURL + `"></script>` +
		`<script src="http://abs.twimg.com/responsive-web/client-web/main.8888888a.js"></script>`
	f := &recordingFetcher{pages: map[string][]byte{xtid.AuthedWarmPageURL: []byte(page)}}

	_, _ = r.build(context.Background(), f, &pageSeen{}) // the bundles carry no ops: rejected, which is fine here

	assert.NotContains(t, f.urls(), wpEvilURL, "an off-allowlist host must never be fetched")
	assert.NotContains(t, f.urls(), "http://abs.twimg.com/responsive-web/client-web/main.8888888a.js", "http must never be fetched")
	assert.Contains(t, f.urls(), wpMainURL)
	assert.Empty(t, bundle.WebpackScripts(`<script src="`+wpEvilURL+`"></script>`, xtid.AuthedWarmPageURL))
}

// recordingFetcher serves canned bodies and records every URL asked for.
type recordingFetcher struct {
	mu    sync.Mutex
	pages map[string][]byte
	got   []string
}

func (f *recordingFetcher) Fetch(_ context.Context, u string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, u)
	if b, ok := f.pages[u]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("not found: %s", u)
}

func (f *recordingFetcher) urls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}
