package main

import (
	"strings"
	"testing"
)

func TestBuildURLAndCounts(t *testing.T) {
	u := buildURL("QID123", "42")
	if !strings.Contains(u, "/graphql/QID123/UserTweetsAndReplies?") || !strings.Contains(u, "42") {
		t.Fatalf("unexpected url %q", u)
	}
	if got := formatCounts(map[int]int{404: 3, 200: 12}); got != "200 x12, 404 x3" {
		t.Fatalf("formatCounts = %q", got)
	}
}

func TestKeyReport(t *testing.T) {
	vs := []variant{{name: "a", key: "ab"}, {name: "b", key: "abcde"}, {name: "c", key: "abcde"}, {name: "d"}}
	if got, want := keyReport(vs), "anim keys: a!=b yes, b!=c no; hex fraction digits dropped by the old path: 3"; got != want {
		t.Fatalf("keyReport = %q, want %q", got, want)
	}
	vs[2].err = "unavailable"
	if got := keyReport(vs); !strings.HasPrefix(got, "anim keys: a!=b yes, b!=c n/a") {
		t.Fatalf("keyReport with unavailable c = %q", got)
	}
}

func TestStopReason(t *testing.T) {
	for _, s := range []int{401, 403, 429} {
		if stopReason(s) == "" {
			t.Errorf("status %d must stop the run", s)
		}
	}
	for _, s := range []int{200, 404, -1} {
		if stopReason(s) != "" {
			t.Errorf("status %d must not stop the run", s)
		}
	}
}
