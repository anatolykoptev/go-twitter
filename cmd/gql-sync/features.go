package main

import (
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
	featureDefaultRe = regexp.MustCompile(`"([a-z0-9_]+)":\{"value":(true|false)\}`)
)

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

// parseFeatureDefaults harvests flag -> bool defaults from a warm-page body.
func parseFeatureDefaults(body []byte) map[string]bool {
	out := make(map[string]bool)
	for _, m := range featureDefaultRe.FindAllSubmatch(body, -1) {
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

// renderFeatures emits the gofmt'd source of features_gen.go: a sorted
// generatedFeatures map of the extracted flags, followed by a // REMOVED comment
// block for any baseline flag that vanished from the bundle. Keys are sorted for
// a stable diff; the optional date stamp is included only when non-empty.
func renderFeatures(features map[string]any, removed map[string]bool, date string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(featuresGeneratedHeader)
	b.WriteString("\n")
	if date != "" {
		b.WriteString("// generated ")
		b.WriteString(date)
		b.WriteString("\n")
	}
	b.WriteString("\npackage twitter\n\n")
	b.WriteString("// generatedFeatures is the GraphQL feature-flag default set extracted from\n")
	b.WriteString("// x.com's bundles (names) and warm-page featureSwitch.defaultConfig (defaults).\n")
	b.WriteString("// gqlFeatures() returns a copy of this when non-empty, else committedFeatures().\n")
	b.WriteString("var generatedFeatures = map[string]any{\n")

	names := make([]string, 0, len(features))
	for name := range features {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString("\t")
		b.WriteString(strconv.Quote(name))
		b.WriteString(": ")
		b.WriteString(renderBool(features[name]))
		b.WriteString(",\n")
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
