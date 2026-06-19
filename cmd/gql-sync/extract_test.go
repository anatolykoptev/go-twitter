package main

import (
	"io"
	"log/slog"
	"maps"
	"testing"
)

func quietExtractor() *extractor {
	return newExtractor(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

const (
	rwAPI    = "https://abs.twimg.com/responsive-web/client-web/api.endpoints.2222222a.js"
	xwebAPI  = "https://abs.twimg.com/x-web/client-web/api.legacy.4444444a.js"
	otherAPI = "https://abs.twimg.com/client-web-legacy/api.5555555a.js"
)

// TestExtractor_BothForms proves a single bundle carrying BOTH op-definition
// shapes yields the union of {name:id} from each regex.
func TestExtractor_BothForms(t *testing.T) {
	body := []byte(`var ops=[{queryId:"AAA111",operationName:"OpOne",metadata:{}}];` +
		`e.exports={n:{params:{id:"BBB222",metadata:{},name:"OpTwo",operationKind:"query"}}};`)

	ext := quietExtractor()
	ext.consume(rwAPI, body)

	got := ext.result()
	want := map[string]string{"OpOne": "AAA111", "OpTwo": "BBB222"}
	if !maps.Equal(got, want) {
		t.Fatalf("result = %v, want %v", got, want)
	}
}

// TestExtractor_SourcePriorityDedupe proves the same op in responsive-web and a
// lower-priority source resolves to the responsive-web id, regardless of the
// order the bundles are walked.
func TestExtractor_SourcePriorityDedupe(t *testing.T) {
	const op = "DupOp"
	rwBody := []byte(`queryId:"RW_WINS",operationName:"DupOp"`)
	otherBody := []byte(`queryId:"OTHER_LOSES",operationName:"DupOp"`)

	t.Run("other seen first, responsive-web overrides", func(t *testing.T) {
		ext := quietExtractor()
		ext.consume(otherAPI, otherBody)
		ext.consume(rwAPI, rwBody)
		if got := ext.result()[op]; got != "RW_WINS" {
			t.Fatalf("%s = %q, want RW_WINS", op, got)
		}
	})

	t.Run("responsive-web seen first, other does not override", func(t *testing.T) {
		ext := quietExtractor()
		ext.consume(rwAPI, rwBody)
		ext.consume(otherAPI, otherBody)
		if got := ext.result()[op]; got != "RW_WINS" {
			t.Fatalf("%s = %q, want RW_WINS", op, got)
		}
	})
}

// TestExtractor_RankOrder proves the full responsive-web > x-web > other order.
func TestExtractor_RankOrder(t *testing.T) {
	const op = "RankedOp"
	ext := quietExtractor()
	ext.consume(otherAPI, []byte(`queryId:"OTHER",operationName:"RankedOp"`))
	ext.consume(xwebAPI, []byte(`queryId:"XWEB",operationName:"RankedOp"`))
	if got := ext.result()[op]; got != "XWEB" {
		t.Fatalf("after other+xweb: %s = %q, want XWEB", op, got)
	}
	ext.consume(rwAPI, []byte(`queryId:"RW",operationName:"RankedOp"`))
	if got := ext.result()[op]; got != "RW" {
		t.Fatalf("after +responsive-web: %s = %q, want RW", op, got)
	}
}

// TestExtractor_BoundedToObjectBoundaries proves the bounded regexes never let
// one op's queryId bind to a different op's operationName on a single-line
// minified bundle. It packs, on ONE line: (a) an op in operationName-before-
// queryId order, (b) an orphan queryId whose object carries no operationName,
// and (c) a params op in name-before-id order. Each op must resolve to its OWN
// id, the orphan must neither steal nor lend an id across an object boundary,
// and no op may be dropped. RED against the old unbounded `.+?` regexes (which
// bind ALPHA111->OpBeta, swallow BETA222, and drop the name-first OpGamma).
func TestExtractor_BoundedToObjectBoundaries(t *testing.T) {
	body := []byte(
		`{operationName:"OpAlpha",queryId:"ALPHA111"},` +
			`{queryId:"ORPHAN999",foo:"bar"},` +
			`{queryId:"BETA222",operationName:"OpBeta"};` +
			`e.exports={n:{params:{name:"OpGamma",id:"GAMMA333",operationKind:"query"}}}`)

	ext := quietExtractor()
	ext.consume(rwAPI, body)

	got := ext.result()
	want := map[string]string{
		"OpAlpha": "ALPHA111",
		"OpBeta":  "BETA222",
		"OpGamma": "GAMMA333",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("result = %v, want %v", got, want)
	}
	// The orphan queryId must not have leaked in as any op's id.
	for name, id := range got {
		if id == "ORPHAN999" {
			t.Fatalf("orphan queryId leaked: %s -> ORPHAN999", name)
		}
	}
}

func TestSourceTag(t *testing.T) {
	cases := map[string]string{
		rwAPI:    srcResponsiveWeb,
		xwebAPI:  srcXWeb,
		otherAPI: srcOther,
	}
	for url, want := range cases {
		if got := sourceTag(url); got != want {
			t.Errorf("sourceTag(%q) = %q, want %q", url, got, want)
		}
	}
}
