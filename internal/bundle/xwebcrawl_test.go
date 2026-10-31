package bundle

import (
	"context"
	"sort"
	"sync/atomic"
	"testing"
)

const xwBase = "https://abs.twimg.com/x-web/x-web/"

// graph models the three linking forms of the Vite build: a static import and a
// mapDeps preload in the entry, a dynamic import and a static import between
// chunks, plus decoys that look like chunk paths.
func xwGraph() *mapFetcher {
	return &mapFetcher{routes: map[string][]byte{
		xwBase + "entry-client-logged-out-AAA.js": []byte(
			`const m=(i=>["assets/preload-1.js","assets/preload-2.js"]);` +
				`import{i as e}from"./assets/runtime-1.js";x=()=>import("./assets/route-1.js")`),
		xwBase + "assets/runtime-1.js": []byte(`export{}`),
		xwBase + "assets/preload-1.js": []byte(`import{a}from"./leaf-1.js"`),
		xwBase + "assets/preload-2.js": []byte(`"assets/preload-2.js"`), // self reference
		xwBase + "assets/route-1.js":   []byte(`import("./leaf-2.js");const e="https://evil.example/x.js"`),
		xwBase + "assets/leaf-1.js":    []byte(`ok`),
		xwBase + "assets/leaf-2.js":    []byte(`ok`),
	}}
}

func crawlURLs(t *testing.T, f Fetcher, opts CrawlOptions) ([]string, CrawlStats) {
	t.Helper()
	var got []string
	st, err := CrawlXWeb(context.Background(), f, []string{xwBase + "entry-client-logged-out-AAA.js"},
		func(u string, _ []byte) { got = append(got, u) }, opts)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got, st
}

func TestCrawlXWeb_FollowsAllThreeLinkForms(t *testing.T) {
	got, st := crawlURLs(t, xwGraph(), CrawlOptions{})
	want := []string{
		xwBase + "assets/leaf-1.js", xwBase + "assets/leaf-2.js", xwBase + "assets/preload-1.js",
		xwBase + "assets/preload-2.js", xwBase + "assets/route-1.js", xwBase + "assets/runtime-1.js",
		xwBase + "entry-client-logged-out-AAA.js",
	}
	if len(got) != len(want) {
		t.Fatalf("visited %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("visited %v\nwant %v", got, want)
		}
	}
	if st.Fetched != 7 {
		t.Errorf("fetched = %d, want 7 (no chunk twice, no foreign host)", st.Fetched)
	}
}

// WalkImports sees only the dynamic imports; this guards the reason CrawlXWeb
// exists.
func TestCrawlXWeb_ReachesMoreThanWalkImports(t *testing.T) {
	var walked []string
	m := &Map{}
	if err := m.WalkImports(context.Background(), xwGraph(), []string{xwBase + "entry-client-logged-out-AAA.js"},
		func(u string, _ []byte) { walked = append(walked, u) }, Options{}); err != nil {
		t.Fatal(err)
	}
	got, _ := crawlURLs(t, xwGraph(), CrawlOptions{})
	if len(got) <= len(walked) {
		t.Errorf("crawl visited %d, WalkImports %d: crawl must reach strictly more", len(got), len(walked))
	}
}

func TestCrawlXWeb_CapAndFailuresAreNotFatal(t *testing.T) {
	g := xwGraph()
	delete(g.routes, xwBase+"assets/leaf-1.js") // dangling literal
	got, st := crawlURLs(t, g, CrawlOptions{})
	if st.Failed != 1 || len(got) != 6 {
		t.Errorf("failed=%d visited=%d, want 1 and 6", st.Failed, len(got))
	}
	_, st = crawlURLs(t, xwGraph(), CrawlOptions{MaxFetch: 3})
	if !st.Capped || st.Fetched+st.Failed != 3 {
		t.Errorf("cap not honoured: %+v", st)
	}
}

func TestCrawlXWeb_DoneStopsEarly(t *testing.T) {
	var polls atomic.Int32
	_, st := crawlURLs(t, xwGraph(), CrawlOptions{Concurrency: 1, Done: func() bool { return polls.Add(1) >= 1 }})
	if !st.Stopped || st.Fetched >= 7 {
		t.Errorf("Done must end the crawl before the graph is exhausted: %+v", st)
	}
}

func TestCrawlXWeb_StartMustBeAllowlisted(t *testing.T) {
	var n int
	st, err := CrawlXWeb(context.Background(), xwGraph(), []string{"https://evil.example/x-web/a.js", "http://abs.twimg.com/x.js"},
		func(string, []byte) { n++ }, CrawlOptions{})
	if err != nil || n != 0 || st.Fetched != 0 {
		t.Errorf("fetched from a disallowed start: n=%d st=%+v err=%v", n, st, err)
	}
}

func TestCrawlXWeb_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CrawlXWeb(ctx, xwGraph(), []string{xwBase + "entry-client-logged-out-AAA.js"}, func(string, []byte) {}, CrawlOptions{})
	if err == nil {
		t.Error("cancelled context must surface")
	}
}
