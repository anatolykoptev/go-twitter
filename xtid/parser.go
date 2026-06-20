package xtid

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	// legacyOnDemandRegex matches the pre-chunk-map direct embed
	// `"ondemand.s":"<hash>"` still present on some x.com snapshots. T1 moved the
	// webpack chunk-map location into internal/bundle, but this direct embed has
	// no chunk map to walk, so xtid keeps it as a fast-path so legacy-only
	// snapshots still resolve. Do NOT drop this without confirming the embed is
	// gone from every served snapshot.
	legacyOnDemandRegex = regexp.MustCompile(`["']ondemand\.s["']\s*:\s*["']([0-9a-f]{6,})["']`)
	// indicesRegex extracts the key-byte indices from the ondemand.s
	// key-derivation expression. Each index appears as a parenthesized radix-16
	// parse of a key-bytes element, e.g. `(n[27],16)` inside
	// `o[l(...)](n[27],16)`. Verified against the real ondemand.s captured
	// 2026-06-19 (hash 91612d9a): the genuine derivation is
	//   let[C,G]=[o[l(...)](n[27],16), ... (n[47],16), (n[47],16), (n[42],16)]
	// so the indices are 27,47,47,42 (rowIndex 27, key-byte indices 47,47,42).
	//
	// The format did NOT change since the upstream form — only the bundle
	// LOCATION moved (now resolved via internal/bundle). The T1 loosening to
	// `\[(\d+)\],\s*16` dropped both discriminators (the wrapping `(...)` and the
	// leading variable char) and OVER-MATCHED: any stray `[N],16` elsewhere in
	// the minified bundle (e.g. `f([5],16)`, a bare `[9],16`, or a 3+ digit
	// non-index) polluted the match list and corrupted rowIndex -> a well-formed
	// but WRONG x-client-transaction-id -> silent 404s. We re-anchor on the
	// observed structure: a `(` open paren, a `\w+` key-bytes variable, the
	// `[index]` access, and the literal `,16` radix, all closed by `)`. `\d{1,3}`
	// caps indices at a real key-byte position while still admitting the longer
	// indices a future bundle could use. See parser_test.go's real-bundle golden.
	indicesRegex = regexp.MustCompile(`\(\w+\[(\d{1,3})\]\s*,\s*16\)`)
)

// onDemandURLTemplate builds the ondemand.s bundle URL from a 7-ish-char hash.
const onDemandURLTemplate = "https://abs.twimg.com/responsive-web/client-web/ondemand.s.%sa.js"

// onDemandLegacyURL returns the ondemand.s bundle URL from the legacy direct
// embed in the warm HTML, or "" when no such embed is present (the modern
// chunk-map case, which Initialize resolves via internal/bundle instead).
func onDemandLegacyURL(html string) string {
	m := legacyOnDemandRegex.FindStringSubmatch(html)
	if len(m) < 2 || m[1] == "" {
		return ""
	}
	return fmt.Sprintf(onDemandURLTemplate, m[1])
}

func getVerificationKey(html string) string {
	re := regexp.MustCompile(`<meta[^>]+name=["']twitter-site-verification["'][^>]+content=["']([^"']+)["']`)
	matches := re.FindStringSubmatch(html)
	if len(matches) > 1 {
		return matches[1]
	}
	re2 := regexp.MustCompile(`<meta[^>]+content=["']([^"']+)["'][^>]+name=["']twitter-site-verification["']`)
	matches = re2.FindStringSubmatch(html)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func getKeyIndices(js string) (int, []int) {
	matches := indicesRegex.FindAllStringSubmatch(js, -1)
	if len(matches) == 0 {
		return 0, nil
	}

	indices := make([]int, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			idx, err := strconv.Atoi(match[1])
			if err == nil {
				indices = append(indices, idx)
			}
		}
	}

	if len(indices) == 0 {
		return 0, nil
	}

	return indices[0], indices[1:]
}

type svgFrame struct {
	id   int
	data [][]int
}

func getSVGFrames(html string) []svgFrame {
	frames := make([]svgFrame, 4)
	for i := 0; i < 4; i++ {
		pattern := regexp.MustCompile(`<svg[^>]*id=["']loading-x-anim-` + strconv.Itoa(i) + `["'][^>]*>[\s\S]*?</svg>`)
		svgMatch := pattern.FindString(html)
		if svgMatch == "" {
			continue
		}

		// Match path with fill="#1d9bf008" — the animation path
		pathPattern := regexp.MustCompile(`<path[^>]*d=["']([^"']+)["'][^>]*fill=["']#1d9bf008["']`)
		pathMatch := pathPattern.FindStringSubmatch(svgMatch)
		if len(pathMatch) < 2 {
			pathPattern2 := regexp.MustCompile(`<path[^>]*fill=["']#1d9bf008["'][^>]*d=["']([^"']+)["']`)
			pathMatch = pathPattern2.FindStringSubmatch(svgMatch)
			if len(pathMatch) < 2 {
				continue
			}
		}

		frames[i] = svgFrame{id: i, data: parsePathData(pathMatch[1])}
	}
	return frames
}

func parsePathData(pathData string) [][]int {
	parts := strings.Split(pathData, "C")
	result := make([][]int, 0, len(parts))
	numRe := regexp.MustCompile(`-?\d+`)
	for idx, part := range parts {
		if idx == 0 {
			continue
		}
		nums := numRe.FindAllString(part, -1)
		if len(nums) == 0 {
			continue
		}
		row := make([]int, 0, len(nums))
		for _, n := range nums {
			val, err := strconv.Atoi(n)
			if err == nil {
				row = append(row, val)
			}
		}
		if len(row) > 0 {
			result = append(result, row)
		}
	}
	return result
}
