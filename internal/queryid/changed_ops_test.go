package queryid

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// fakeCatalog treats every id other than "compiled-<op>" as changed, except
// for env-pinned operations, which never count.
func fakeCatalog(pinned ...string) Catalog {
	pin := map[string]bool{}
	for _, p := range pinned {
		pin[p] = true
	}
	return Catalog{
		Operations: func() []string { return nil },
		Known:      func(string) bool { return true },
		CountChanged: func(ids map[string]string) int {
			n := 0
			for op, id := range ids {
				if id != "compiled-"+op && !pin[op] {
					n++
				}
			}
			return n
		},
	}
}

func captureApplied(t *testing.T, cat Catalog, ids map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	r := New(Config{Catalog: cat})
	r.noteSuccess("test", &Snapshot{IDs: ids, Changed: cat.CountChanged(ids)}, 0)
	return buf.String()
}

func TestNoteSuccess_LogsSortedChangedOpNamesNotIDs(t *testing.T) {
	ids := map[string]string{
		"Zeta": "SECRETZ", "Alpha": "SECRETA", "Same": "compiled-Same", "Pinned": "SECRETP",
	}
	out := captureApplied(t, fakeCatalog("Pinned"), ids)
	if !strings.Contains(out, "changed=2") {
		t.Fatalf("count missing: %s", out)
	}
	if !strings.Contains(out, `changed_ops="[Alpha Zeta]"`) {
		t.Fatalf("want sorted changed names without Same/Pinned: %s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("ids leaked into the log: %s", out)
	}
}

func TestNoteSuccess_CapsChangedOpNames(t *testing.T) {
	ids := map[string]string{}
	for i := 0; i < maxLoggedChangedOps+5; i++ {
		ids[fmt.Sprintf("Op%03d", i)] = "new"
	}
	out := captureApplied(t, fakeCatalog(), ids)
	if !strings.Contains(out, "changed_ops_more=5") {
		t.Fatalf("overflow count missing: %s", out)
	}
	if strings.Contains(out, fmt.Sprintf("Op%03d", maxLoggedChangedOps)) || !strings.Contains(out, "Op029") {
		t.Fatalf("cap not applied at %d names: %s", maxLoggedChangedOps, out)
	}
}

func TestNoteSuccess_NoChangeNoNames(t *testing.T) {
	out := captureApplied(t, fakeCatalog(), map[string]string{"A": "compiled-A"})
	if strings.Contains(out, "changed_ops") {
		t.Fatalf("unexpected names on an unchanged refresh: %s", out)
	}
}
