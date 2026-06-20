package main

import (
	"bytes"
	"fmt"
	"go/format"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	// defaultConfig object (see sliceDefaultConfig + parseFeatureDefaults), never
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

// featureCollector accumulates the per-op feature NAMES across every walked
// bundle body. It mirrors the queryID extractor's consume(url, body) shape so a
// single WalkImports can fan a body into both passes.
type featureCollector struct {
	names map[string]struct{}
}

func newFeatureCollector() *featureCollector {
	return &featureCollector{names: make(map[string]struct{})}
}

// consume harvests every feature-name array in one bundle body. bundleURL is
// unused (names are not source-ranked like queryIDs) but kept for the shared
// visit signature.
func (c *featureCollector) consume(_ string, body []byte) {
	for _, m := range featureNameRe.FindAllSubmatch(body, -1) {
		for _, tok := range featureTokenRe.FindAllSubmatch(m[1], -1) {
			c.names[string(tok[1])] = struct{}{}
		}
	}
}

// result returns the harvested feature-name set, sorted.
func (c *featureCollector) result() []string {
	out := make([]string, 0, len(c.names))
	for n := range c.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// parseFeatureDefaults harvests flag -> bool guest defaults from a warm-page
// body. The scan is bounded to the featureSwitch.defaultConfig object; if that
// anchor is missing or unbalanced it returns an empty map (the caller's
// empty-extraction guard then leaves features_gen.go untouched rather than
// shipping a mis-scoped set) instead of falling back to a clobber-prone
// whole-body scan.
func parseFeatureDefaults(body []byte) map[string]bool {
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

// intersectFeatures builds the emitted name->default map from the bundle-declared
// names and the warm-page defaults. A bundle name with no default is skipped and
// logged (a feature the op sends but the warm page does not default — emitting it
// with an invented value would be a guess). Returns the map plus the skipped
// names for reporting.
func intersectFeatures(names []string, defaults map[string]bool, log *slog.Logger) (map[string]any, []string) {
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

// removedFeatures returns the baseline flags absent from the freshly extracted
// set. They are NEVER dropped silently: the emitter surfaces them as a // REMOVED
// comment so the operator decides — deleting a flag x.com still requires is an
// HTTP 400 on every call.
func removedFeatures(extracted map[string]any, baseline map[string]bool) map[string]bool {
	removed := make(map[string]bool)
	for name, val := range baseline {
		if _, ok := extracted[name]; !ok {
			removed[name] = val
		}
	}
	return removed
}

// baselineLineRe reads a committed/generated features map back into a baseline
// set. It matches the emitter's own `"name": true,` line shape and intentionally
// ignores // REMOVED comment lines (they are comments, not live entries).
var baselineLineRe = regexp.MustCompile(`(?m)^\s*"([a-z0-9_]+)":\s+(true|false),`)

// parseFeatureBaseline reads the prior generatedFeatures (or the committed
// literal) from a features_gen.go body into a name->bool baseline.
func parseFeatureBaseline(body []byte) map[string]bool {
	out := make(map[string]bool)
	for _, m := range baselineLineRe.FindAllSubmatch(body, -1) {
		out[string(m[1])] = string(m[2]) == "true"
	}
	return out
}

const featuresGeneratedHeader = "// Code generated by cmd/gql-sync; DO NOT EDIT."

// newFeatureComment marks a flag NEW to the committed set, so the operator
// scrutinizes a guest-sourced default before trusting it on authed calls.
const newFeatureComment = " — NEW — verify against an AUTHED capture before trusting this default"

// renderFeatures emits the gofmt'd source of features_gen.go. The extraction is
// a NAME-SET discovery (bundle names ∩ warm-page defaults); VALUES come from
// `committed` for every KNOWN flag — committed values are the authed-session
// authority, the warm page is logged-out and its guest defaults legitimately
// differ. Each line carries the warm-page guest default as a trailing comment so
// the operator sees x.com's current guest signal WITHOUT it becoming the wire
// value. A flag absent from `committed` is NEW: it adopts the guest default and
// is flagged for an authed-capture verification. A // REMOVED comment block
// follows for any committed flag that vanished from the bundle. Keys are sorted
// for a stable diff; the optional date stamp is included only when non-empty.
func renderFeatures(extracted map[string]any, committed map[string]bool, removed map[string]bool, date string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(featuresGeneratedHeader)
	b.WriteString("\n")
	if date != "" {
		b.WriteString("// generated ")
		b.WriteString(date)
		b.WriteString("\n")
	}
	b.WriteString("\npackage twitter\n\n")
	b.WriteString("// generatedFeatures is the GraphQL feature-flag set: NAMES discovered from\n")
	b.WriteString("// x.com's bundles ∩ warm-page featureSwitch.defaultConfig, with VALUES taken\n")
	b.WriteString("// from committedFeatures() for every KNOWN flag (committed values are the\n")
	b.WriteString("// authed-session authority — the warm page is logged-out and its guest\n")
	b.WriteString("// defaults legitimately differ). The guest default is shown as a trailing\n")
	b.WriteString("// comment on every line; a flag NEW to the committed set adopts the guest\n")
	b.WriteString("// default and is flagged for an authed-capture verification.\n")
	b.WriteString("// gqlFeatures() returns a copy of this when non-empty, else committedFeatures().\n")
	b.WriteString("var generatedFeatures = map[string]any{\n")

	names := make([]string, 0, len(extracted))
	for name := range extracted {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		guest := extracted[name]
		b.WriteString("\t")
		b.WriteString(strconv.Quote(name))
		b.WriteString(": ")
		if cv, known := committed[name]; known {
			// Committed value WINS for known flags; guest default is informational.
			b.WriteString(renderBool(cv))
			b.WriteString(", // warm-page guest default: ")
			b.WriteString(renderBool(guest))
		} else {
			// NEW flag: no committed authority — adopt the guest default, flagged.
			b.WriteString(renderBool(guest))
			b.WriteString(", // warm-page guest default: ")
			b.WriteString(renderBool(guest))
			b.WriteString(newFeatureComment)
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")

	writeRemovedBlock(&b, removed)

	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("gofmt generated features: %w", err)
	}
	return formatted, nil
}

// renderBool renders a feature value as a Go bool literal. Values are always bool
// (parseFeatureDefaults only stores bools); a non-bool slips through as %v rather
// than panicking, surfacing the anomaly in the diff.
func renderBool(v any) string {
	if b, ok := v.(bool); ok {
		return strconv.FormatBool(b)
	}
	return fmt.Sprintf("%v", v)
}

// writeRemovedBlock appends the // REMOVED comment block (sorted) when any
// baseline flag vanished from the latest extraction.
func writeRemovedBlock(b *strings.Builder, removed map[string]bool) {
	if len(removed) == 0 {
		return
	}
	names := make([]string, 0, len(removed))
	for name := range removed {
		names = append(names, name)
	}
	sort.Strings(names)

	b.WriteString("\n// REMOVED — present in the prior baseline but absent from the latest bundle\n")
	b.WriteString("// extraction. NOT auto-deleted: a flag x.com still requires is an HTTP 400 on\n")
	b.WriteString("// every call. Review and restore (re-add to committedFeatures) or delete.\n")
	for _, name := range names {
		b.WriteString("//\t")
		b.WriteString(name)
		b.WriteString(" (was ")
		b.WriteString(strconv.FormatBool(removed[name]))
		b.WriteString(")\n")
	}
}
