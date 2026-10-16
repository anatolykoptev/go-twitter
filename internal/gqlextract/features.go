package gqlextract

import (
	"bytes"
	"log/slog"
	"regexp"
	"sort"
)

// Feature-flag extraction (T4). Twitter's GraphQL requests carry a per-operation
// `features` object — flag name -> boolean. The bundles and warm page split that
// signal across two REAL locations (verified against a live x.com capture, NOT a
// synthetic fixture):
//
//   - NAMES live in the bundles as per-op arrays `[...,"flag_enabled"],fieldToggles:[...]`
//     (the exact list each GraphQL op sends). featureNameRe harvests them while
//     WalkImports streams the same bundle bodies the queryID pass consumes.
//   - DEFAULTS live in the warm page's `window.__INITIAL_STATE__.featureSwitch.defaultConfig`
//     as `"flag_name":{"value":<bool>}` entries. They are NOT in any JS chunk —
//     the spec assumed a JS default-config chunk; the real client ships defaults
//     in the page's initial state. featureDefaultRe harvests them.
//
// The emitted map is the INTERSECTION: a flag is generated only when the bundles
// declare it AND the warm page gives it a boolean default. That intersection is
// self-bounding — defaultConfig also carries ~2000 non-GraphQL switches, but only
// names a GraphQL op actually sends survive, so the output stays the focused
// feature set (never the whole switch registry).
var (
	// featureNameRe matches a per-op feature-name array terminated by ,fieldToggles:.
	// The {3,} floor keeps it off incidental short string arrays. The names are
	// captured as one blob (group 1) and split by featureTokenRe.
	featureNameRe  = regexp.MustCompile(`\[((?:"[a-z0-9_]+",?){3,})\],fieldToggles:`)
	featureTokenRe = regexp.MustCompile(`"([a-z0-9_]+)"`)

	// featureDefaultRe matches the warm-page default-config shape
	// `"flag_name":{"value":true|false}`. The {"value":...} wrapper is required, so
	// bare initial-state booleans (e.g. "profile_sort_enabled":true) never match.
	// Non-bool switches (country whitelists: "...":{"value":[...]}) also never match.
	// IMPORTANT: this regex is scanned ONLY within the balanced featureSwitch.
	// defaultConfig object (see sliceDefaultConfig + ParseFeatureDefaults), never
	// the whole page body — a same-named "...":{"value":bool} elsewhere in the
	// initial state would otherwise clobber the real default (FindAll map assignment
	// is last-write-wins).
	featureDefaultRe = regexp.MustCompile(`"([a-z0-9_]+)":\{"value":(true|false)\}`)
)

// defaultConfigKey marks the warm-page object that holds the GraphQL feature
// defaults: window.__INITIAL_STATE__.featureSwitch.defaultConfig.
const defaultConfigKey = `"defaultConfig":`

// sliceDefaultConfig returns the balanced-brace JSON object that follows the
// first featureSwitch.defaultConfig key, or nil when the key/object is absent or
// unbalanced. Brace counting skips braces inside double-quoted strings (honoring
// backslash escapes) so nested {"value":...} entries and string payloads never
// miscount. Anchoring the default scan here de-risks the last-write-wins clobber
// the review flagged: a stray "...":{"value":bool} outside defaultConfig is no
// longer in scope.
func sliceDefaultConfig(body []byte) []byte {
	idx := bytes.Index(body, []byte(defaultConfigKey))
	if idx < 0 {
		return nil
	}
	rest := body[idx+len(defaultConfigKey):]
	open := bytes.IndexByte(rest, '{')
	if open < 0 {
		return nil
	}
	depth := 0
	inStr := false
	esc := false
	for i := open; i < len(rest); i++ {
		ch := rest[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[open : i+1]
			}
		}
	}
	return nil // unbalanced — bail rather than scan a truncated object
}

// FeatureCollector accumulates the per-op feature NAMES across every walked
// bundle body. It mirrors the queryID extractor's Consume(url, body) shape so a
// single WalkImports can fan a body into both passes.
type FeatureCollector struct {
	names map[string]struct{}
}

// NewFeatureCollector returns an empty FeatureCollector.
func NewFeatureCollector() *FeatureCollector {
	return &FeatureCollector{names: make(map[string]struct{})}
}

// Consume harvests every feature-name array in one bundle body. bundleURL is
// unused (names are not source-ranked like queryIDs) but kept for the shared
// visit signature.
func (c *FeatureCollector) Consume(_ string, body []byte) {
	for _, m := range featureNameRe.FindAllSubmatch(body, -1) {
		for _, tok := range featureTokenRe.FindAllSubmatch(m[1], -1) {
			c.names[string(tok[1])] = struct{}{}
		}
	}
}

// Result returns the harvested feature-name set, sorted.
func (c *FeatureCollector) Result() []string {
	out := make([]string, 0, len(c.names))
	for n := range c.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ParseFeatureDefaults harvests flag -> bool guest defaults from a warm-page
// body. The scan is bounded to the featureSwitch.defaultConfig object; if that
// anchor is missing or unbalanced it returns an empty map (the caller's
// empty-extraction guard then leaves features_gen.go untouched rather than
// shipping a mis-scoped set) instead of falling back to a clobber-prone
// whole-body scan.
func ParseFeatureDefaults(body []byte) map[string]bool {
	out := make(map[string]bool)
	scope := sliceDefaultConfig(body)
	if scope == nil {
		return out
	}
	for _, m := range featureDefaultRe.FindAllSubmatch(scope, -1) {
		out[string(m[1])] = string(m[2]) == "true"
	}
	return out
}

// IntersectFeatures builds the emitted name->default map from the bundle-declared
// names and the warm-page defaults. A bundle name with no default is skipped and
// logged (a feature the op sends but the warm page does not default — emitting it
// with an invented value would be a guess). Returns the map plus the skipped
// names for reporting.
func IntersectFeatures(names []string, defaults map[string]bool, log *slog.Logger) (map[string]any, []string) {
	out := make(map[string]any, len(names))
	var noDefault []string
	for _, n := range names {
		v, ok := defaults[n]
		if !ok {
			noDefault = append(noDefault, n)
			continue
		}
		out[n] = v
	}
	if len(noDefault) > 0 {
		log.Warn("gql-sync: bundle feature names with no warm-page default (skipped)",
			slog.Any("names", noDefault))
	}
	return out, noDefault
}
