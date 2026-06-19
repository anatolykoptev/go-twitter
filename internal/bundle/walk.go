package bundle

import (
	"context"
	"log/slog"
	"net/url"
)

// WalkImports fetches the start bundles and BFS-follows import("./X.js") refs,
// invoking visit for each successfully fetched (url, body). It is bounded by
// opts.MaxBundles and logs a bundle.bfs_capped warning with the drop count when
// the cap is hit. Per-bundle fetch errors are logged and skipped (graceful
// degradation), never aborting the whole walk. Used by T3 (queryID extraction)
// and T4 (feature extraction).
func (m *Map) WalkImports(ctx context.Context, f Fetcher, start []string,
	visit func(url string, body []byte), opts Options) error {
	opts.applyDefaults()
	seen := make(map[string]bool, len(start))
	queue := dedupeQueue(start, seen)

	fetched := 0
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fetched >= opts.MaxBundles {
			slog.Warn("bundle.bfs_capped",
				slog.Int("max_bundles", opts.MaxBundles),
				slog.Int("fetched", fetched),
				slog.Int("dropped", len(queue)))
			break
		}

		current := queue[0]
		queue = queue[1:]

		body, err := f.Fetch(ctx, current)
		if err != nil {
			slog.Warn("bundle: walk fetch failed",
				slog.String("url", current), slog.Any("error", err))
			continue
		}
		fetched++
		visit(current, body)
		queue = enqueueImports(queue, seen, current, body)
	}
	return nil
}

// dedupeQueue seeds the BFS queue from start, marking each as seen.
func dedupeQueue(start []string, seen map[string]bool) []string {
	queue := make([]string, 0, len(start))
	for _, s := range start {
		if seen[s] {
			continue
		}
		seen[s] = true
		queue = append(queue, s)
	}
	return queue
}

// enqueueImports appends every not-yet-seen import edge found in body to queue.
func enqueueImports(queue []string, seen map[string]bool, parent string, body []byte) []string {
	for _, match := range importRe.FindAllSubmatch(body, -1) {
		child, ok := resolveImport(parent, string(match[1]))
		if !ok || seen[child] {
			continue
		}
		seen[child] = true
		queue = append(queue, child)
	}
	return queue
}

// resolveImport resolves a relative import (e.g. "foo.js") against the parent
// bundle URL's directory.
func resolveImport(parent, rel string) (string, bool) {
	base, err := url.Parse(parent)
	if err != nil {
		return "", false
	}
	ref, err := url.Parse("./" + rel)
	if err != nil {
		return "", false
	}
	return base.ResolveReference(ref).String(), true
}
