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

func (c *diskCache) store(m *Map) error {
	if err := os.MkdirAll(c.dir, cacheDirPerm); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal chunk map: %w", err)
	}
	if err := os.WriteFile(c.path(), data, cacheFilePerm); err != nil {
		return fmt.Errorf("write cache file: %w", err)
	}
	return nil
}
