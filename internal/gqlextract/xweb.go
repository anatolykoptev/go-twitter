package gqlextract

import (
	"log/slog"
	"regexp"
	"sort"
	"strings"
)

// x-web (Vite + Relay) extraction.
//
// x.com's new web client ships one Relay artifact per operation, compiled into
// its own chunk:
//
//	{fragment:{...,selections:[...]},kind:`Request`,
//	 operation:{argumentDefinitions:[...],...},
//	 params:{id:`<queryId>`,metadata:{},name:`<operationName>`,
//	         operationKind:`query`,text:null,providedVariables:{...}}}
//
// The legacy extractor (Extractor) reads `queryId:"…",operationName:"…"` and a
// quoted `params:{id:"…",name:"…",operationKind` shape; neither matches here
// (template-literal strings, key order id/metadata/name). This file parses the
// artifact structurally instead of by regex so key order and nested values do
// not matter.

// Stack tags an Op with the web stack it was extracted from.
const (
	StackLegacy = "legacy" // responsive-web / queryId+operationName shape
	StackXWeb   = "x-web"  // Vite + Relay artifact
)

// providerPrefix marks Relay provided variables (feature-flag-like constants the
// web client injects; the request must carry them as plain variables).
const providerPrefix = "__relay_internal__pv__"

// Op is one extracted GraphQL operation with the stack it came from.
type Op struct {
	Name string
	ID   string
	Kind string // "query" | "mutation" ("" when the source does not say)
	// Stack is StackLegacy or StackXWeb.
	Stack string
	// Source is the chunk/bundle URL the op was read from.
	Source string
	// Variables are the declared operation arguments, excluding Relay providers.
	// Only populated for x-web ops (legacy bundles do not declare them).
	Variables []string
	// Providers are the __relay_internal__pv__* provided variables (x-web only).
	Providers []string
	// Features are feature-switch names. x-web Relay artifacts declare none (the
	// web client sends no `features` parameter), so this is always nil there.
	Features []string
	// ResponseRoot is the chain of single linked fields at the top of the
	// response selection, e.g. "viewer_v2.user_results.result.tweet_notification_timeline"
	// (x-web only). A level with several sibling fields ends the chain as
	// "{a,b}".
	ResponseRoot string
}

var (
	paramsOpenRe  = regexp.MustCompile(`\bparams:\{`)
	artifactOpen  = regexp.MustCompile(`\{fragment:\{`)
	maxChainDepth = 24
)

// ParseRelayArtifacts returns every Relay operation artifact in a chunk body.
// A body without one (the vast majority of chunks) returns nil. The returned
// ops carry the format-validated id only: an id failing QueryIDRe is skipped.
func ParseRelayArtifacts(source string, body []byte) []Op {
	text := string(body)
	var ops []Op
	prevEnd := 0
	for _, loc := range paramsOpenRe.FindAllStringIndex(text, -1) {
		open := loc[1] - 1
		if open < prevEnd {
			continue // inside an artifact already consumed
		}
		end := matchClose(text, open)
		if end < 0 {
			continue
		}
		op, ok := buildOp(source, text, prevEnd, open, end)
		prevEnd = end + 1
		if ok {
			ops = append(ops, op)
		}
	}
	return ops
}

// buildOp builds the Op for the params object text[open:end+1], using text from
// lo for the enclosing artifact and its argument declarations.
func buildOp(source, text string, lo, open, end int) (Op, bool) {
	params := objectFields(text[open : end+1])
	id, okID := stringField(params, "id")
	name, okName := stringField(params, "name")
	kind, okKind := stringField(params, "operationKind")
	if !okID || !okName || !okKind || !QueryIDRe.MatchString(id) {
		return Op{}, false // e.g. a router `params:{id:`requests`}` or a tampered id
	}
	op := Op{Name: name, ID: id, Kind: kind, Stack: StackXWeb, Source: source}
	if pv, ok := fieldVal(params, "providedVariables"); ok && strings.HasPrefix(pv, "{") {
		for _, f := range objectFields(pv) {
			op.Providers = appendUnique(op.Providers, f.key)
		}
	}

	art, start := enclosingArtifact(text, lo, open)
	if art == "" {
		return op, true
	}
	artFields := objectFields(art)
	res := newResolver(text[lo:start])
	if opRaw, ok := fieldVal(artFields, "operation"); ok {
		opFields := objectFields(opRaw)
		if defs, ok := fieldVal(opFields, "argumentDefinitions"); ok {
			for _, el := range arrayElems(res.literal(defs)) {
				n := res.argName(el)
				switch {
				case n == "":
				case strings.HasPrefix(n, providerPrefix):
					op.Providers = appendUnique(op.Providers, n)
				default:
					op.Variables = appendUnique(op.Variables, n)
				}
			}
		}
	}
	if frag, ok := fieldVal(artFields, "fragment"); ok {
		if sel, ok := fieldVal(objectFields(frag), "selections"); ok {
			op.ResponseRoot = responseRoot(res, sel)
		}
	}
	return op, true
}

// enclosingArtifact returns the `{fragment:{…},kind:…,operation:{…},params:{…}}`
// object whose params open at `open`, searching back no further than lo.
func enclosingArtifact(text string, lo, open int) (string, int) {
	locs := artifactOpen.FindAllStringIndex(text[lo:open], -1)
	// Aliased inline fragments inside the selections also read `{fragment:{`, so
	// take the nearest candidate that actually encloses the params object.
	for i := len(locs) - 1; i >= 0; i-- {
		start := lo + locs[i][0]
		end := matchClose(text, start)
		if end >= open {
			return text[start : end+1], start
		}
	}
	return "", 0
}

// resolver looks up the minifier-assigned variables an artifact declares in the
// span before its return (`var s={…},n=[…];`), so identifier references inside
// argumentDefinitions and selections can be followed to their literal.
type resolver struct {
	span  string
	cache map[string]string
}

func newResolver(span string) *resolver { return &resolver{span: span, cache: map[string]string{}} }

// lookup returns the raw object/array literal ident is assigned in the span, or
// "" when it is not declared there.
func (r *resolver) lookup(ident string) string {
	if v, ok := r.cache[ident]; ok {
		return v
	}
	out := ""
	if validIdent(ident) {
		re := regexp.MustCompile(`(?:^|[^\w$.])` + regexp.QuoteMeta(ident) + `=([\[{])`)
		// The nearest preceding declaration is the artifact's own: short minified
		// names are reused across scopes earlier in the chunk.
		if locs := re.FindAllStringSubmatchIndex(r.span, -1); len(locs) > 0 {
			loc := locs[len(locs)-1]
			open := loc[3] - 1
			if end := matchClose(r.span, open); end >= 0 {
				out = r.span[open : end+1]
			}
		}
	}
	r.cache[ident] = out
	return out
}

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// literal returns el itself when it is an object/array literal, else the
// literal the identifier el was assigned.
func (r *resolver) literal(el string) string {
	if strings.HasPrefix(el, "{") || strings.HasPrefix(el, "[") {
		return el
	}
	return r.lookup(el)
}

// argName resolves one argumentDefinitions element to its argument name.
func (r *resolver) argName(el string) string {
	n, _ := stringField(objectFields(r.literal(el)), "name")
	return n
}

// responseRoot walks the fragment selections down the chain of single linked
// fields. Inline fragments are looked through (their fields join the level);
// identifier references (shared selections) end the walk.
func responseRoot(res *resolver, selections string) string {
	var path []string
	sel := selections
	for depth := 0; depth < maxChainDepth; depth++ {
		linked, others := levelFields(res, sel)
		switch {
		case len(linked) == 1 && len(others) == 0:
			path = append(path, linked[0].name)
			sel = linked[0].selections
			if sel == "" {
				return strings.Join(path, ".")
			}
		default:
			names := make([]string, 0, len(linked)+len(others))
			for _, l := range linked {
				names = append(names, l.name)
			}
			names = append(names, others...)
			if len(names) > 0 {
				path = append(path, "{"+strings.Join(names, ",")+"}")
			}
			return strings.Join(path, ".")
		}
	}
	return strings.Join(path, ".")
}

type linkedField struct{ name, selections string }

// levelFields lists the LinkedFields of a selections array (looking through
// inline fragments) and the names of its non-linked members (scalars).
func levelFields(res *resolver, selections string) (linked []linkedField, others []string) {
	for _, el := range arrayElems(res.literal(selections)) {
		el = res.literal(el)
		if !strings.HasPrefix(el, "{") {
			continue
		}
		fs := objectFields(el)
		kind, _ := stringField(fs, "kind")
		switch kind {
		case "InlineFragment":
			if inner, ok := fieldVal(fs, "selections"); ok {
				l, o := levelFields(res, inner)
				linked = append(linked, l...)
				others = append(others, o...)
			}
		case "LinkedField":
			n, _ := stringField(fs, "name")
			inner, _ := fieldVal(fs, "selections")
			linked = append(linked, linkedField{name: n, selections: inner})
		case "ScalarField":
			n, _ := stringField(fs, "name")
			others = append(others, n)
		}
	}
	return linked, others
}

func stringField(fs []field, key string) (string, bool) {
	raw, ok := fieldVal(fs, key)
	if !ok {
		return "", false
	}
	return stringLit(raw)
}

func appendUnique(s []string, v string) []string {
	for _, e := range s {
		if e == v {
			return s
		}
	}
	return append(s, v)
}

// XWebExtractor accumulates x-web Relay operations across chunks. The first id
// seen for an operation name wins; a differing later id is logged and kept in
// Conflicts so a surprising second artifact is never silent.
type XWebExtractor struct {
	ops       map[string]Op
	conflicts map[string][]string
	log       *slog.Logger
}

// NewXWebExtractor returns an empty extractor logging to logger (nil =
// slog.Default).
func NewXWebExtractor(logger *slog.Logger) *XWebExtractor {
	if logger == nil {
		logger = slog.Default()
	}
	return &XWebExtractor{ops: map[string]Op{}, conflicts: map[string][]string{}, log: logger}
}

// Consume parses one chunk body and records every Relay artifact in it.
func (x *XWebExtractor) Consume(source string, body []byte) {
	for _, op := range ParseRelayArtifacts(source, body) {
		prev, seen := x.ops[op.Name]
		if !seen {
			x.ops[op.Name] = op
			continue
		}
		if prev.ID != op.ID {
			x.conflicts[op.Name] = appendUnique(x.conflicts[op.Name], op.ID)
			x.log.Warn("x-web op id conflict: first kept",
				slog.String("op", op.Name), slog.String("kept", prev.ID), slog.String("dropped", op.ID))
			continue
		}
		// Same id seen twice: a `*_parameters` chunk carries params only while
		// the .graphql chunk carries the full artifact, so fill what is missing.
		if prev.ResponseRoot == "" && op.ResponseRoot != "" {
			prev.ResponseRoot = op.ResponseRoot
		}
		if len(prev.Variables) == 0 {
			prev.Variables = op.Variables
		}
		for _, p := range op.Providers {
			prev.Providers = appendUnique(prev.Providers, p)
		}
		x.ops[op.Name] = prev
	}
}

// Ops returns the extracted operations sorted by name.
func (x *XWebExtractor) Ops() []Op {
	out := make([]Op, 0, len(x.ops))
	for _, op := range x.ops {
		out = append(out, op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Result returns operation name -> query id.
func (x *XWebExtractor) Result() map[string]string {
	out := make(map[string]string, len(x.ops))
	for n, op := range x.ops {
		out[n] = op.ID
	}
	return out
}

// Conflicts returns, per operation name, the extra ids seen beyond the kept one.
func (x *XWebExtractor) Conflicts() map[string][]string { return x.conflicts }
