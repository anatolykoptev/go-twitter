package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	cacheFileName             = "chunkmap.json"
	cacheDirPerm  os.FileMode = 0o755
	cacheFilePerm os.FileMode = 0o644
)

// diskCache persists a single reassembled Map snapshot as JSON under dir.
type diskCache struct {
	dir string
	ttl time.Duration
}

func (c *diskCache) path() string { return filepath.Join(c.dir, cacheFileName) }

// load returns the cached map only if present and still within the TTL relative
// to now.
func (c *diskCache) load(now time.Time) (*Map, bool) {
	m, ok := c.read()
	if !ok {
		return nil, false
	}
	if now.Sub(m.FetchedAt) > c.ttl {
		return nil, false
	}
	return m, true
}

// loadStale returns the cached map ignoring the TTL — the degraded fallback used
// when a live fetch fails.
func (c *diskCache) loadStale() (*Map, bool) { return c.read() }

func (c *diskCache) read() (*Map, bool) {
	data, err := os.ReadFile(c.path())
	if err != nil {
		return nil, false
	}
	var m Map
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	return &m, true
}

// store writes the snapshot atomically: a unique temp file in the same dir is
// fully written then os.Rename'd over the final path. A torn write or two
// concurrent Builds can never leave a partial chunkmap.json behind.
func (c *diskCache) store(m *Map) error {
	if err := os.MkdirAll(c.dir, cacheDirPerm); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal chunk map: %w", err)
	}

	tmp, err := os.CreateTemp(c.dir, cacheFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp cache file: %w", err)
	}
	if err := os.Chmod(tmpName, cacheFilePerm); err != nil {
		return fmt.Errorf("chmod temp cache file: %w", err)
	}
	if err := os.Rename(tmpName, c.path()); err != nil {
		return fmt.Errorf("rename cache file: %w", err)
	}
	return nil
}
