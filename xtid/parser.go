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
	// indicesRegex extracts the key-byte indices (`[N], 16`). Loosened per twikit
	// PR #411 (was `\(\w{1}\[(\d{1,2})\],\s*16\)`) so post-Mar-2026 bundles with
	// 3+ digit indices and no wrapping `(x[…])` still match.
	indicesRegex = regexp.MustCompile(`\[(\d+)\],\s*16`)
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
