package main

import (
	"log/slog"
	"regexp"
	"strings"
)

// Extraction regexes (twscrape reference — exact). Run over every bundle body
// fetched by bundle.Map.WalkImports.
//
//   - opRe matches the modern { queryId, operationName } op-definition shape.
//   - paramsRe matches the persisted-query params:{id,name,…operationKind} shape.
var (
	opRe     = regexp.MustCompile(`queryId:["'](.+?)["'].+?operationName:["'](.+?)["']`)
	paramsRe = regexp.MustCompile(`params:\{id:["']([^"']+)["'].+?name:["']([^"']+)["'].+?operationKind`)
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

// extractor accumulates operationName -> queryId across every walked bundle,
// deduping by source priority and logging every genuine collision so a
// surprising override is never silent.
type extractor struct {
	ops map[string]extractedOp
	log *slog.Logger
}

func newExtractor(logger *slog.Logger) *extractor {
	if logger == nil {
		logger = slog.Default()
	}
	return &extractor{ops: make(map[string]extractedOp), log: logger}
}

// consume runs both op-definition regexes over a single bundle body and records
// every (operationName, queryId) pair found, tagged with the bundle's source.
func (e *extractor) consume(bundleURL string, body []byte) {
	tag := sourceTag(bundleURL)
	rank := sourceRank(tag)
	text := string(body)

	for _, m := range opRe.FindAllStringSubmatch(text, -1) {
		e.add(m[2], m[1], tag, rank) // m[1]=queryId, m[2]=operationName
	}
	for _, m := range paramsRe.FindAllStringSubmatch(text, -1) {
		e.add(m[2], m[1], tag, rank) // m[1]=id, m[2]=name
	}
}

// add records name->id, resolving collisions by source priority. The first hit
// for a name wins ties within the same rank; a higher-ranked source overrides a
// lower-ranked one. Every genuine collision (same name, different id) is logged.
func (e *extractor) add(name, id, src string, rank int) {
	if name == "" || id == "" {
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

// result returns the resolved operationName -> queryId map.
func (e *extractor) result() map[string]string {
	out := make(map[string]string, len(e.ops))
	for name, op := range e.ops {
		out[name] = op.id
	}
	return out
}
