package bundle

import (
	"fmt"
	"regexp"
	"time"
)

// Chunk-map regexes (twscrape reference — exact). nameRe also matches
// hash-shaped values because a 7-hex hash is a valid [\w.\-]+; parseChunkMap
// drops those (via hashOnly) so Names holds module names only.
var (
	nameRe   = regexp.MustCompile(`(\d+):"([\w.\-]+)"`)    // chunkID -> name
	hashRe   = regexp.MustCompile(`(\d+):"([0-9a-f]{7})"`) // chunkID -> 7-hex hash
	importRe = regexp.MustCompile(`import\(\s*["']\.\/([\w.\-]+\.js)["']`)
	hashOnly = regexp.MustCompile(`^[0-9a-f]{7}$`)
)

const bundleURLTemplate = "https://abs.twimg.com/responsive-web/client-web/%s.%sa.js"

// Map is one reassembled snapshot of x.com's webpack chunk map.
type Map struct {
	Names     map[string]string `json:"names"`  // chunkID -> module name (e.g. "20113" -> "ondemand.s")
	Hashes    map[string]string `json:"hashes"` // chunkID -> 7-hex content hash
	FetchedAt time.Time         `json:"fetched_at"`
}

// ChunkIDByName finds the chunkID whose module name == name (e.g. "ondemand.s").
func (m *Map) ChunkIDByName(name string) (string, bool) {
	for id, n := range m.Names {
		if n == name {
			return id, true
		}
	}
	return "", false
}

// BundleURL resolves a module name to its abs.twimg.com bundle URL of the form
// https://abs.twimg.com/responsive-web/client-web/{name}.{hash}a.js
func (m *Map) BundleURL(name string) (string, bool) {
	id, ok := m.ChunkIDByName(name)
	if !ok {
		return "", false
	}
	hash, ok := m.Hashes[id]
	if !ok {
		return "", false
	}
	return fmt.Sprintf(bundleURLTemplate, name, hash), true
}

// parseChunkMap reassembles the chunkID->name and chunkID->hash dicts from a
// bundle/HTML body. The two dicts share chunkID keys, so BundleURL joins a
// name's chunkID to its hash.
func parseChunkMap(body string) (names, hashes map[string]string) {
	names = make(map[string]string)
	hashes = make(map[string]string)
	for _, match := range hashRe.FindAllStringSubmatch(body, -1) {
		hashes[match[1]] = match[2]
	}
	for _, match := range nameRe.FindAllStringSubmatch(body, -1) {
		if hashOnly.MatchString(match[2]) {
			continue // hash-shaped value belongs to Hashes, not Names
		}
		names[match[1]] = match[2]
	}
	return names, hashes
}

func mergeInto(dst, src map[string]string) {
	for k, v := range src {
		dst[k] = v
	}
}
