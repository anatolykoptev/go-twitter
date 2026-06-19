package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func goldenQueryIDs() map[string]string {
	return map[string]string{
		"UserByScreenName": "IGgvgiOx4QZndDHuD3x9TQ",
		"SearchTimeline":   "GcXk9vN_d1jUfHNqLacXQA",
		"TweetDetail":      "VWFGPVAGkZMGRKGe3GFFnA",
		"CreateTweet":      "7TKRKCPuAGsmYde0CudbVg",
	}
}

func readGolden(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "queryids_gen.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return data
}

// TestRenderQueryIDs_Golden proves the emitter output (gofmt'd, sorted, header)
// is byte-identical to the committed golden.
func TestRenderQueryIDs_Golden(t *testing.T) {
	got, err := renderQueryIDs(goldenQueryIDs(), "")
	if err != nil {
		t.Fatalf("renderQueryIDs: %v", err)
	}
	want := readGolden(t)
	if !bytes.Equal(got, want) {
		t.Fatalf("emitter output != golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderQueryIDs_Deterministic proves the same map renders byte-identically
// regardless of Go map iteration order.
func TestRenderQueryIDs_Deterministic(t *testing.T) {
	first, err := renderQueryIDs(goldenQueryIDs(), "")
	if err != nil {
		t.Fatalf("render 1: %v", err)
	}
	for i := 0; i < 8; i++ {
		next, err := renderQueryIDs(goldenQueryIDs(), "")
		if err != nil {
			t.Fatalf("render %d: %v", i+2, err)
		}
		if !bytes.Equal(first, next) {
			t.Fatal("emitter output is not deterministic across runs")
		}
	}
}

// TestRenderQueryIDs_DateStamp proves the -date stamp is included only when
// non-empty, and otherwise leaves the content-only output untouched.
func TestRenderQueryIDs_DateStamp(t *testing.T) {
	withDate, err := renderQueryIDs(goldenQueryIDs(), "2026-06-19")
	if err != nil {
		t.Fatalf("render with date: %v", err)
	}
	if !strings.Contains(string(withDate), "// generated 2026-06-19") {
		t.Fatal("date stamp missing when -date is set")
	}
	noDate := readGolden(t)
	if strings.Contains(string(noDate), "// generated") {
		t.Fatal("golden (no date) must not carry a generation stamp")
	}
}

// TestGeneratedHeaderMarker guards the code-generation marker convention.
func TestGeneratedHeaderMarker(t *testing.T) {
	if !strings.HasPrefix(generatedHeader, "// Code generated ") ||
		!strings.HasSuffix(generatedHeader, " DO NOT EDIT.") {
		t.Fatalf("generatedHeader %q breaks the Go code-generated convention", generatedHeader)
	}
}
