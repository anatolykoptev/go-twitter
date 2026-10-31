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
