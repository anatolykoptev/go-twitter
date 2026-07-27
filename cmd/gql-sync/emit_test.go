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

// TestParseGeneratedQueryIDs proves the parser recovers the operationName ->
// queryId map from a rendered queryids_gen.go source, enabling the additive
// merge in syncQueryIDs (issue #39).
func TestParseGeneratedQueryIDs(t *testing.T) {
	rendered, err := renderQueryIDs(goldenQueryIDs(), "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := parseGeneratedQueryIDs(rendered)
	want := goldenQueryIDs()
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parsed[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestMergeCommittedQueryIDs_PreservesSessionOnly proves the merge keeps
// committed entries that the extraction did NOT re-extract (session-only ops
// gql-sync cannot reach from public bundles), while overriding entries that
// x.com rotated.
func TestMergeCommittedQueryIDs_PreservesSessionOnly(t *testing.T) {
	dir := t.TempDir()
	// Seed the on-disk file with a committed baseline that includes a
	// session-only op (UserTweets) and a rotated op (SearchTimeline).
	committed := map[string]string{
		"UserTweets":     "FOlovQsiHGDls3c0Q_HaSQ", // session-only — NOT in extraction
		"SearchTimeline": "OLD_ROTATED_ID",         // rotated — extraction has a new ID
		"Followers":      "PqHIuVqkBBkxmeTnE1f1Yg", // unchanged — extraction has same ID
	}
	body, err := renderQueryIDs(committed, "")
	if err != nil {
		t.Fatalf("render committed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, generatedFileName), body, 0o644); err != nil {
		t.Fatalf("write committed: %v", err)
	}

	// Extraction: SearchTimeline rotated, Followers unchanged, UserTweets absent.
	extracted := map[string]string{
		"SearchTimeline": "GcXk9vN_d1jUfHNqLacXQA",
		"Followers":      "PqHIuVqkBBkxmeTnE1f1Yg",
	}

	cfg := config{out: dir}
	merged := mergeCommittedQueryIDs(cfg, extracted)

	// UserTweets preserved from committed (session-only).
	if id, ok := merged["UserTweets"]; !ok || id != "FOlovQsiHGDls3c0Q_HaSQ" {
		t.Errorf("UserTweets: got %q (ok=%v), want preserved %q", id, ok, "FOlovQsiHGDls3c0Q_HaSQ")
	}
	// SearchTimeline overridden with fresh extraction.
	if id := merged["SearchTimeline"]; id != "GcXk9vN_d1jUfHNqLacXQA" {
		t.Errorf("SearchTimeline: got %q, want rotated %q", id, "GcXk9vN_d1jUfHNqLacXQA")
	}
	// Followers present from both, same ID.
	if id := merged["Followers"]; id != "PqHIuVqkBBkxmeTnE1f1Yg" {
		t.Errorf("Followers: got %q, want %q", id, "PqHIuVqkBBkxmeTnE1f1Yg")
	}
	if len(merged) != 3 {
		t.Errorf("merged has %d entries, want 3", len(merged))
	}
}

// TestMergeCommittedQueryIDs_NoPriorFile proves the merge is a no-op (returns
// the extraction as-is) when no prior queryids_gen.go exists (first run).
func TestMergeCommittedQueryIDs_NoPriorFile(t *testing.T) {
	dir := t.TempDir()
	extracted := map[string]string{"UserTweets": "FOlovQsiHGDls3c0Q_HaSQ"}
	cfg := config{out: dir}
	merged := mergeCommittedQueryIDs(cfg, extracted)
	if len(merged) != 1 || merged["UserTweets"] != "FOlovQsiHGDls3c0Q_HaSQ" {
		t.Fatalf("no prior file: merged = %v, want extraction as-is", merged)
	}
}
