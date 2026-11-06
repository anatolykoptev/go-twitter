package queryid

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

const (
	layoutWebpackPage = `<html><script src="https://abs.twimg.com/responsive-web/client-web/main.0123456789abcdefa.js"></script></html>`
	layoutVitePage    = `<html><script type="module" src="https://abs.twimg.com/x-web/x-web/entry-client-0123456789abcdef.js"></script></html>`
	layoutBothPage    = layoutWebpackPage + layoutVitePage
	layoutBlankPage   = `<html><body>no scripts here</body></html>`
)

// alertLog collects the pool alerts a refresher emits.
type alertLog struct {
	mu     sync.Mutex
	topics []string
	pay    []any
}

func (a *alertLog) hook(topic string, payload any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.topics = append(a.topics, topic)
	a.pay = append(a.pay, payload)
}

func (a *alertLog) count(topic string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, t := range a.topics {
		if t == topic {
			n++
		}
	}
	return n
}

func newLayoutRefresher(t *testing.T) (*Refresher, *alertLog) {
	t.Helper()
	r := New(Config{Catalog: fakeCatalog()})
	log := &alertLog{}
	r.SetAlertHook(log.hook)
	return r, log
}

// pageFetcher serves one fixed warm page for every URL.
type pageFetcher struct{ page string }

func (p pageFetcher) Fetch(context.Context, string) ([]byte, error) { return []byte(p.page), nil }

// observe feeds pages through the xtid hook path; fetch feeds them through the
// refresher's own page fetch (the only path when xtid never fetches the
// authenticated page). Both must reach noteLayout.
var layoutFeeders = map[string]func(t *testing.T, r *Refresher, page string){
	"ObservePage": func(_ *testing.T, r *Refresher, page string) { r.ObservePage([]byte(page)) },
	"pageFor": func(t *testing.T, r *Refresher, page string) {
		t.Helper()
		r.Mu.Lock()
		r.page = nil // no xtid-observed page: pageFor must fetch its own
		r.Mu.Unlock()
		if _, err := r.pageFor(context.Background(), pageFetcher{page}); err != nil {
			t.Fatal(err)
		}
	},
}

// The alert fires once per webpack -> vite transition, never on a repeat, and
// never on the first observation after the process starts.
//
// Mutation A: noteLayout, delete the `from != LayoutWebpack` check => RED
// (fires on every vite page). Mutation B: never call poolAlert => RED.
// Mutation C: treat "no previous layout" as webpack (from := LayoutWebpack
// when r.layout == "") => RED on the first-observation case.
// Mutation D: remove the noteLayout call in pageFor => RED on the pageFor
// subtests (no xtid hook in play).
func TestNoteLayout_AlertsOncePerWebpackToViteTransition(t *testing.T) {
	cases := []struct {
		name  string
		pages []string
		want  int
	}{
		{"webpack, webpack, vite, vite fires once", []string{layoutWebpackPage, layoutWebpackPage, layoutVitePage, layoutVitePage}, 1},
		{"first observation vite never fires", []string{layoutVitePage, layoutVitePage}, 0},
		{"first observation both never fires", []string{layoutBothPage}, 0},
		{"webpack only never fires", []string{layoutWebpackPage, layoutWebpackPage}, 0},
		{"webpack to both fires", []string{layoutWebpackPage, layoutBothPage}, 1},
		{"vite back to webpack to vite fires on the second flip only", []string{layoutWebpackPage, layoutVitePage, layoutWebpackPage, layoutVitePage}, 2},
		{"an unrecognised page neither fires nor forgets webpack", []string{layoutWebpackPage, layoutBlankPage, layoutVitePage}, 1},
		{"unknown first, then vite, never fires", []string{layoutBlankPage, layoutVitePage}, 0},
	}
	for feeder, feed := range layoutFeeders {
		for _, tc := range cases {
			t.Run(feeder+"/"+tc.name, func(t *testing.T) {
				r, log := newLayoutRefresher(t)
				for _, p := range tc.pages {
					feed(t, r, p)
				}
				if got := log.count("queryid.layout_changed"); got != tc.want {
					t.Fatalf("layout_changed alerts = %d, want %d (topics %v)", got, tc.want, log.topics)
				}
			})
		}
	}
}

func TestNoteLayout_AlertPayloadNamesLayouts(t *testing.T) {
	r, log := newLayoutRefresher(t)
	r.ObservePage([]byte(layoutWebpackPage))
	r.ObservePage([]byte(layoutVitePage))
	if len(log.pay) != 1 {
		t.Fatalf("alerts = %d, want 1", len(log.pay))
	}
	got, _ := log.pay[0].(map[string]any)
	if got["from"] != LayoutWebpack || got["to"] != LayoutVite {
		t.Fatalf("payload = %v, want from=webpack to=vite", log.pay[0])
	}
}

// With no hook wired the alert still surfaces as a log line, never a nil call.
func TestNoteLayout_NoHookDoesNotPanic(t *testing.T) {
	r := New(Config{Catalog: fakeCatalog()})
	r.ObservePage([]byte(layoutWebpackPage))
	r.ObservePage([]byte(layoutVitePage))
}

// A disabled refresher observes nothing.
func TestNoteLayout_DisabledObservesNothing(t *testing.T) {
	r := New(Config{Catalog: fakeCatalog(), Disabled: true})
	log := &alertLog{}
	r.SetAlertHook(log.hook)
	r.ObservePage([]byte(layoutWebpackPage))
	r.ObservePage([]byte(layoutVitePage))
	if n := log.count("queryid.layout_changed"); n != 0 {
		t.Fatalf("alerts = %d, want 0", n)
	}
}

func readQIDFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The real inline entry script of the x-web page shell (minimised from the
// public page) must parse as a Vite page whose first script is the entry URL,
// and that entry is the logged-out shell. A signed-in entry name is an
// assumption (entry-client-<hash>.js): it must NOT read as the shell.
//
// Mutation: bundle/vite.go ViteScripts, drop xWebScriptRe from refs => RED
// (the inline entryHref is the only reference on this page).
func TestParseWarmPage_RealInlineEntryScript(t *testing.T) {
	wp := ParseWarmPage(readQIDFixture(t, "entry-inline.html"), time.Unix(0, 0))
	if wp.Layout != LayoutVite {
		t.Fatalf("layout = %q, want %q", wp.Layout, LayoutVite)
	}
	const entry = "https://abs.twimg.com/x-web/x-web/entry-client-logged-out-CtNHf5M-.js"
	if len(wp.Vite) == 0 || wp.Vite[0] != entry {
		t.Fatalf("Vite = %v, want [%s ...]", wp.Vite, entry)
	}
	if !bundle.IsLoggedOutShell(wp.Scripts) {
		t.Fatal("entry-client-logged-out-* must be the logged-out shell")
	}
	if bundle.IsLoggedOutShell([]string{"https://abs.twimg.com/x-web/x-web/entry-client-CtNHf5M-.js"}) {
		t.Fatal("entry-client-<hash>.js must not be the logged-out shell")
	}
}

// The logged-out shell reaches the xtid hook whenever an account's cookies are
// refused. It is a dead session, not the signed-in build: it must not fire the
// pool-wide alert, and must not overwrite the stored layout (else the next
// healthy webpack page would reset the transition silently).
//
// Mutation: noteLayout, drop `|| bundle.IsLoggedOutShell(wp.Scripts)` => RED.
func TestNoteLayout_LoggedOutShellIsIgnored(t *testing.T) {
	shell := string(readQIDFixture(t, "entry-inline.html"))
	for feeder, feed := range layoutFeeders {
		t.Run(feeder, func(t *testing.T) {
			r, log := newLayoutRefresher(t)
			feed(t, r, layoutWebpackPage)
			feed(t, r, shell)
			if n := log.count("queryid.layout_changed"); n != 0 {
				t.Fatalf("alerts after a logged-out shell = %d, want 0", n)
			}
			if got := r.lastLayout(); got != LayoutWebpack {
				t.Fatalf("stored layout = %q, want %q", got, LayoutWebpack)
			}
			feed(t, r, layoutVitePage) // a real flip afterwards still alerts, once
			if n := log.count("queryid.layout_changed"); n != 1 {
				t.Fatalf("alerts after a real flip = %d, want 1", n)
			}
		})
	}
}
