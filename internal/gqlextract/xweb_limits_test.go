package gqlextract

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Hostile-input bounds. Each body is ~0.5 MB and targets ONE quadratic path; the
// parse must finish in well under the bound (it takes milliseconds when the cap
// holds, tens of seconds when it does not). The call runs in a goroutine so a
// regression fails the test instead of hanging it.
const limitBound = 2 * time.Second

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
	within(t, "unclosed params", strings.Repeat("params:{id:`a`,metadata:{", 20000), 0)
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
	body := strings.Repeat("{fragment:{argumentDefinitions:[", 16000) +
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
