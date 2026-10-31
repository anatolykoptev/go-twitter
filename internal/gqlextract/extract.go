// Package gqlextract extracts GraphQL queryIDs and feature flags from x.com's
// own bundles. It is the single implementation shared by cmd/gql-sync (the
// weekly codegen) and the runtime queryId refresh in the twitter package, so
// the two can never disagree on what a bundle says.
package gqlextract

import (
	"log/slog"
	"regexp"
	"sort"
	"strings"
)

// Extraction regexes. Run over every bundle body fetched by
// bundle.Map.WalkImports.
//
// Basis: bounded community queryID extractors. (twscrape itself hardcodes its
// queryIDs and ships no extraction regex, so it is NOT the reference.) Every
// pattern bridges fields only WITHIN a single object via braceSpan — never
// across the object-closing `}` — so on a real single-line minified bundle one
// op's queryId can never bind to a different op's operationName. That mis-bind
// is invisible to the dedupe (it's a wrong name->id, not a same-name collision).
//
//   - opReFwd/opReRev match the { queryId, operationName } op-definition shape
//     in EITHER field order (real bundles vary), so no op is dropped on order.
//   - paramsReIDFirst/paramsReNameFirst match the persisted-query
//     params:{id,name,…operationKind} shape in EITHER id/name order.
//
// braceSpan bridges fields inside one object: any run of non-brace chars,
// optionally crossing a nested single-level object (e.g. metadata:{}), but never
// the object-closing `}`. A nested `metadata:{}` legitimately sits between id
// and name in real params objects, so a flat `[^}]*?` would wrongly drop those
// ops; braceSpan tolerates the nested braces while still refusing to bleed past
// the boundary.
const braceSpan = `(?:[^{}]|\{[^{}]*\})*?`

var (
	opReFwd = regexp.MustCompile(`queryId:["']([^"']+)["']` + braceSpan + `operationName:["']([^"']+)["']`)
	opReRev = regexp.MustCompile(`operationName:["']([^"']+)["']` + braceSpan + `queryId:["']([^"']+)["']`)

	paramsReIDFirst   = regexp.MustCompile(`params:\{id:["']([^"']+)["']` + braceSpan + `name:["']([^"']+)["']` + braceSpan + `operationKind`)
	paramsReNameFirst = regexp.MustCompile(`params:\{name:["']([^"']+)["']` + braceSpan + `id:["']([^"']+)["']` + braceSpan + `operationKind`)

	// QueryIDRe bounds a captured queryId to the exact alphabet x.com uses for
	// persisted-query IDs. A poisoned bundle could otherwise smuggle a value
	// carrying `/`, a quote, or whitespace that later flows unescaped into the
	// request path. A non-matching value is DROPPED (the op falls back to its
	// committed ID), never overridden.
	QueryIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Source-priority ranks. When the same operationName resolves to different
// queryIds across bundles, the highest-ranked source wins:
// responsive-web > x-web > other.
const (
	srcResponsiveWeb = "responsive-web"
	srcXWeb          = "x-web"
	srcOther         = "other"

	rankResponsiveWeb = 3
	rankXWeb          = 2
	rankOther         = 1
)

// sourceTag derives the source tag from a bundle URL path.
func sourceTag(bundleURL string) string {
	switch {
	case strings.Contains(bundleURL, "/"+srcResponsiveWeb+"/"):
		return srcResponsiveWeb
	case strings.Contains(bundleURL, "/"+srcXWeb+"/"):
		return srcXWeb
	default:
		return srcOther
	}
}

func sourceRank(tag string) int {
	switch tag {
	case srcResponsiveWeb:
		return rankResponsiveWeb
	case srcXWeb:
		return rankXWeb
	default:
		return rankOther
	}
}

// extractedOp is one resolved operation plus the source it was won from, so a
// later lower-priority hit can be dropped (and logged) deterministically.
type extractedOp struct {
	id   string
	rank int
	src  string
}

// Extractor accumulates operationName -> queryId across every walked bundle,
// deduping by source priority and logging every genuine collision so a
// surprising override is never silent.
type Extractor struct {
	ops map[string]extractedOp
	// invalid records every operation whose queryId failed QueryIDRe, so a
	// caller that must not trust a tampered bundle can reject the extraction
	// instead of silently using the remaining ops.
	invalid map[string]struct{}
	log     *slog.Logger
	// queryIDShapeOnly restricts extraction to the `{queryId, operationName}`
	// op-definition shape (see Option QueryIDShapeOnly).
	queryIDShapeOnly bool
}

// Option tunes an Extractor.
type Option func(*Extractor)

// QueryIDShapeOnly makes the extractor ignore the persisted-query
// `params:{id,name,operationKind}` shape and keep only the
// `{queryId, operationName}` shape. The params shape is where Relay
// persisted-query HASHES live — a different identifier than the GraphQL-path
// queryId that, used as a path segment, answers HTTP 422
// GRAPHQL_VALIDATION_FAILED (see the engagement-mutation note in endpoints.go).
// A runtime consumer that swaps ids into live requests uses this so such a
// hash can never be mistaken for a queryId; the offline codegen keeps both
// shapes and a human reviews its diff.
func QueryIDShapeOnly() Option {
	return func(e *Extractor) { e.queryIDShapeOnly = true }
}

// NewExtractor returns an empty Extractor logging to logger (nil = slog.Default).
func NewExtractor(logger *slog.Logger, opts ...Option) *Extractor {
	if logger == nil {
		logger = slog.Default()
	}
	e := &Extractor{ops: make(map[string]extractedOp), invalid: make(map[string]struct{}), log: logger}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Consume runs both op-definition regexes over a single bundle body and records
// every (operationName, queryId) pair found, tagged with the bundle's source.
func (e *Extractor) Consume(bundleURL string, body []byte) {
	tag := sourceTag(bundleURL)
	rank := sourceRank(tag)
	text := string(body)

	// opRe runs before paramsRe so that, at equal source rank, an op-definition
	// hit wins ties over a params hit (add() keeps the first hit per rank). This
	// tiebreak is intentional.
	for _, m := range opReFwd.FindAllStringSubmatch(text, -1) {
		e.add(m[2], m[1], tag, rank) // m[1]=queryId, m[2]=operationName
	}
	for _, m := range opReRev.FindAllStringSubmatch(text, -1) {
		e.add(m[1], m[2], tag, rank) // m[1]=operationName, m[2]=queryId
	}
	if e.queryIDShapeOnly {
		return
	}
	for _, m := range paramsReIDFirst.FindAllStringSubmatch(text, -1) {
		e.add(m[2], m[1], tag, rank) // m[1]=id, m[2]=name
	}
	for _, m := range paramsReNameFirst.FindAllStringSubmatch(text, -1) {
		e.add(m[1], m[2], tag, rank) // m[1]=name, m[2]=id
	}
}

// add records name->id, resolving collisions by source priority. The first hit
// for a name wins ties within the same rank; a higher-ranked source overrides a
// lower-ranked one. Every genuine collision (same name, different id) is logged.
func (e *Extractor) add(name, id, src string, rank int) {
	if name == "" || id == "" {
		return
	}
	if !QueryIDRe.MatchString(id) {
		e.invalid[name] = struct{}{}
		e.log.Warn("queryid dropped: failed format validation",
			slog.String("op", name),
			slog.String("id_preview", truncate(id, 32)),
			slog.String("src", src))
		return
	}
	prev, seen := e.ops[name]
	if !seen {
		e.ops[name] = extractedOp{id: id, rank: rank, src: src}
		return
	}
	if prev.id == id {
		return // same id from another bundle — not a collision worth logging
	}
	if rank > prev.rank {
		e.log.Info("queryid collision: higher-priority source wins",
			slog.String("op", name),
			slog.String("kept_id", id), slog.String("kept_src", src),
			slog.String("dropped_id", prev.id), slog.String("dropped_src", prev.src))
		e.ops[name] = extractedOp{id: id, rank: rank, src: src}
		return
	}
	e.log.Info("queryid collision: lower-priority source dropped",
		slog.String("op", name),
		slog.String("kept_id", prev.id), slog.String("kept_src", prev.src),
		slog.String("dropped_id", id), slog.String("dropped_src", src))
}

// truncate caps a string to at most n runes for safe logging of an
// untrusted/oversized value (never log the whole bundle slice).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Result returns the resolved operationName -> queryId map.
func (e *Extractor) Result() map[string]string {
	out := make(map[string]string, len(e.ops))
	for name, op := range e.ops {
		out[name] = op.id
	}
	return out
}

// Invalid reports whether any occurrence of operation name carried a queryId
// that failed QueryIDRe (and was therefore dropped from Result).
func (e *Extractor) Invalid(name string) bool {
	_, bad := e.invalid[name]
	return bad
}

// Ops returns every legacy-shape operation the extractor resolved, tagged
// StackLegacy, sorted by name. It exists so a combined inventory can report
// which stack each id came from.
func (e *Extractor) Ops() []Op {
	out := make([]Op, 0, len(e.ops))
	for name, op := range e.ops {
		out = append(out, Op{Name: name, ID: op.id, Stack: StackLegacy, Source: op.src})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
