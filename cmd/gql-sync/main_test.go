package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestRun_FixturesMatchesGolden proves the end-to-end CLI (build chunk map ->
// WalkImports -> extract -> emit) regenerates a queryids_gen.go byte-identical
// to the committed golden — the acceptance gate for the offline path.
func TestRun_FixturesMatchesGolden(t *testing.T) {
	out := t.TempDir()
	if err := run([]string{"-fixtures", "testdata", "-out", out}); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(out, generatedFileName))
	if err != nil {
		t.Fatalf("read generated: %v", err)
	}
	want := readGolden(t)
	if !bytes.Equal(got, want) {
		t.Fatalf("generated != golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

const sentinel = "package twitter\n\n// SENTINEL — must remain untouched on an empty extraction.\n"

// TestRun_FailOnEmpty proves an empty extraction with -fail-on-empty exits
// non-zero AND leaves the target file untouched (never clobbers on a break).
func TestRun_FailOnEmpty(t *testing.T) {
	out := t.TempDir()
	target := filepath.Join(out, generatedFileName)
	if err := os.WriteFile(target, []byte(sentinel), generatedFilePerm); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	err := run([]string{"-fixtures", filepath.Join("testdata", "empty"), "-out", out, "-fail-on-empty"})
	if err == nil {
		t.Fatal("expected a non-nil error on empty extraction with -fail-on-empty")
	}

	after, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target: %v", readErr)
	}
	if string(after) != sentinel {
		t.Fatalf("target was modified on empty extraction:\n%s", after)
	}
}

// TestRun_EmptyWithoutFlagLeavesFileUntouched proves that even without
// -fail-on-empty an empty extraction never writes (exit 0, file unchanged).
func TestRun_EmptyWithoutFlagLeavesFileUntouched(t *testing.T) {
	out := t.TempDir()
	target := filepath.Join(out, generatedFileName)
	if err := os.WriteFile(target, []byte(sentinel), generatedFilePerm); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	if err := run([]string{"-fixtures", filepath.Join("testdata", "empty"), "-out", out}); err != nil {
		t.Fatalf("run (no fail flag) should not error on empty: %v", err)
	}

	after, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target: %v", readErr)
	}
	if string(after) != sentinel {
		t.Fatalf("target was modified on empty extraction without -fail-on-empty:\n%s", after)
	}
}

// TestRun_NoRewriteWhenUnchanged proves a second run over identical fixtures
// leaves the already-current file byte-identical (idempotent, no churn).
func TestRun_NoRewriteWhenUnchanged(t *testing.T) {
	out := t.TempDir()
	args := []string{"-fixtures", "testdata", "-out", out}
	if err := run(args); err != nil {
		t.Fatalf("first run: %v", err)
	}
	target := filepath.Join(out, generatedFileName)
	first, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after first run: %v", err)
	}
	if err := run(args); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("second run changed the up-to-date file")
	}
}

// TestParseFlags_Defaults guards the documented flag defaults.
func TestParseFlags_Defaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.out != "." || cfg.proxy != "" || cfg.fixtures != "" || cfg.date != "" || cfg.failOnEmpty {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}
