package gqlextract

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// Hostile-input bounds. Each body is 0.5-1 MB and targets ONE quadratic path; the
// parse must finish in well under the bound (it takes milliseconds when the cap
// holds, tens of seconds when it does not). The call runs in a goroutine so a
// regression fails the test instead of hanging it.
const limitBound = 6 * time.Second

func within(t *testing.T, name string, body string, want int) {
	t.Helper()
	done := make(chan []Op, 1)
	start := time.Now()
	go func() { done <- ParseRelayArtifacts("u", []byte(body)) }()
	select {
	case ops := <-done:
		if len(ops) != want {
			t.Errorf("%s: %d ops, want %d", name, len(ops), want)
		}
		t.Logf("%s: %d bytes in %v", name, len(body), time.Since(start))
	case <-time.After(limitBound):
		t.Fatalf("%s: %d-byte body still parsing after %v (quadratic scan)", name, len(body), limitBound)
	}
}

// params:{ openers whose brace never closes: each unbounded scan runs to EOF.
func TestParseRelayArtifacts_UnclosedParamsIsBounded(t *testing.T) {
	within(t, "unclosed params", strings.Repeat("params:{id:`a`,metadata:{", 40000), 0)
}

// Thousands of declared identifiers all referenced from argumentDefinitions.
func TestParseRelayArtifacts_ManyIdentifiersIsBounded(t *testing.T) {
	const n = 8000
	var decl, refs strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&decl, "i%d={defaultValue:null,kind:`LocalArgument`,name:`a%d`},", i, i)
		fmt.Fprintf(&refs, "i%d,", i)
	}
	body := "x=(function(){var " + decl.String() + "z=1;return{fragment:{argumentDefinitions:[],kind:`Fragment`,name:`N`,selections:[]}," +
		"kind:`Request`,operation:{argumentDefinitions:[" + refs.String() + "],kind:`Operation`,name:`N`,selections:[]}," +
		"params:{id:`idN`,metadata:{},name:`N`,operationKind:`query`,text:null}}})();"
	within(t, "many identifiers", body, 1)
}

// Repeated unclosed `{fragment:{argumentDefinitions:` openers before one params.
func TestParseRelayArtifacts_RepeatedFragmentOpenersIsBounded(t *testing.T) {
	body := strings.Repeat("{fragment:{argumentDefinitions:[", 32000) +
		"params:{id:`idN`,metadata:{},name:`N`,operationKind:`query`,text:null}"
	within(t, "repeated fragment openers", body, 1)
}

func TestParseRelayArtifacts_OversizedBodyIsRefused(t *testing.T) {
	big := "params:{id:`idN`,metadata:{},name:`N`,operationKind:`query`,text:null}" + strings.Repeat(" ", MaxChunkBytes)
	within(t, "oversized", big, 0)
	if got := len(ParseRelayArtifacts("u", []byte(big[:MaxChunkBytes]))); got != 1 {
		t.Errorf("a body at the cap must still parse, got %d ops", got)
	}
}

func TestParseRelayArtifactsContext_StopsBetweenArtifacts(t *testing.T) {
	body := ""
	for i := 0; i < 50; i++ {
		body += fmt.Sprintf("p%d={params:{id:`id%d`,metadata:{},name:`N%d`,operationKind:`query`,text:null}};", i, i, i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ops := ParseRelayArtifactsContext(ctx, "u", []byte(body)); len(ops) != 0 {
		t.Errorf("cancelled context still parsed %d artifacts", len(ops))
	}
	if ops := ParseRelayArtifactsContext(context.Background(), "u", []byte(body)); len(ops) != 50 {
		t.Errorf("live context parsed %d artifacts, want 50", len(ops))
	}
}

// Every repeat leaves one fragment opener unclosed and adds one balanced params,
// so no per-object cap helps: each opener scans to EOF. Only the per-body work
// budget bounds this (a 1 MiB body took 46.8 s before it, growth quadratic).
//
// Mutation: in parseRelay make the budget unlimited (newScan returns a huge
// left) => RED.
func TestParseRelayArtifacts_InterleavedOpenersAndParamsIsBounded(t *testing.T) {
	rep := "{fragment:{argumentDefinitions:[params:{id:`idN`,name:`N`,operationKind:`query`}"
	body := strings.Repeat(rep, 1<<20/len(rep)) // ~1 MiB
	done := make(chan struct{})
	var ex *XWebExtractor
	start := time.Now()
	go func() {
		ex = NewXWebExtractor(nil)
		ex.Consume("u", []byte(body))
		close(done)
	}()
	select {
	case <-done:
		t.Logf("%d bytes in %v, truncated=%d", len(body), time.Since(start), ex.Truncated())
	case <-time.After(limitBound):
		t.Fatalf("interleaved openers: %d-byte body still parsing after %v (quadratic scan)", len(body), limitBound)
	}
	if ex.Truncated() != 1 {
		t.Errorf("the budget stop must be counted: truncated=%d, want 1", ex.Truncated())
	}
}

// A hostile providedVariables / argument list must not make de-duplication
// quadratic nor grow the Op without bound.
func TestParseRelayArtifacts_HugeNameListsAreCapped(t *testing.T) {
	var pv, args strings.Builder
	for i := 0; i < 8000; i++ {
		fmt.Fprintf(&pv, "__relay_internal__pv__p%d:x,", i)
		fmt.Fprintf(&args, "{defaultValue:null,kind:`LocalArgument`,name:`v%d`},", i)
	}
	body := "x={fragment:{argumentDefinitions:[],kind:`Fragment`,name:`N`,selections:[]},kind:`Request`," +
		"operation:{argumentDefinitions:[" + args.String() + "],kind:`Operation`,name:`N`,selections:[]}," +
		"params:{id:`idN`,metadata:{},name:`N`,operationKind:`query`,text:null,providedVariables:{" + pv.String() + "}}}"
	done := make(chan []Op, 1)
	go func() { done <- ParseRelayArtifacts("u", []byte(body)) }()
	select {
	case ops := <-done:
		if len(ops) != 1 || len(ops[0].Variables) > maxArtifactNames || len(ops[0].Providers) > maxArtifactNames {
			t.Fatalf("lists not capped: %d ops, %d vars, %d providers", len(ops), len(ops[0].Variables), len(ops[0].Providers))
		}
	case <-time.After(limitBound):
		t.Fatalf("%d-byte name lists still parsing after %v", len(body), limitBound)
	}
}

// Random soups of the tokens the parser reacts to, not just the shapes found by
// hand: whatever the interleaving, parsing stays within the bound.
func TestParseRelayArtifacts_RandomShapesStayBounded(t *testing.T) {
	toks := []string{
		"{fragment:{argumentDefinitions:[", "params:{id:`idN`,name:`N`,operationKind:`query`}", "{", "[", "}", "]", "(", ")",
		"`", "'", "operation:{argumentDefinitions:[a,b,c],selections:[", "selections:[{kind:`InlineFragment`,", "kind:`LinkedField`,name:`f`,",
		"a={defaultValue:null,kind:`LocalArgument`,name:`x`},", "b=[", "providedVariables:{__relay_internal__pv__p:x},", ",", ":",
		"params:{", "{fragment:{kind:`InlineFragment`,", "kind:`Request`,",
	}
	for seed := int64(1); seed <= 24; seed++ {
		r := rand.New(rand.NewSource(seed))
		var b strings.Builder
		for b.Len() < 256<<10 {
			b.WriteString(toks[r.Intn(len(toks))])
		}
		body := b.String()
		done := make(chan struct{})
		start := time.Now()
		go func() { ParseRelayArtifacts("u", []byte(body)); close(done) }()
		select {
		case <-done:
		case <-time.After(limitBound):
			t.Fatalf("seed %d: %d-byte random body still parsing after %v", seed, len(body), time.Since(start))
		}
	}
}
