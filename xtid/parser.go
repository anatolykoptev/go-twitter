package xtid

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// Legacy format: "ondemand.s":"<hash>"
	onDemandLegacyRegex = regexp.MustCompile(`['|"]{1}ondemand\.s['|"]{1}:\s*['|"]{1}([\w]*)['|"]{1}`)
	// New webpack format: chunk_id:"ondemand.s" (name map)
	onDemandChunkRegex = regexp.MustCompile(`(\d+)\s*:\s*["']ondemand\.s["']`)
	indicesRegex       = regexp.MustCompile(`\(\w{1}\[(\d{1,2})\],\s*16\)`)
)

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

func getOnDemandFileURL(html string) string {
	// Try legacy format first: "ondemand.s":"<hash>"
	matches := onDemandLegacyRegex.FindStringSubmatch(html)
	if len(matches) > 1 && matches[1] != "" {
		return "https://abs.twimg.com/responsive-web/client-web/ondemand.s." + matches[1] + "a.js"
	}

	// New webpack format: find chunk ID, then look up hash
	chunkMatch := onDemandChunkRegex.FindStringSubmatch(html)
	if len(chunkMatch) < 2 {
		return ""
	}
	chunkID := chunkMatch[1]

	// Find hash for this chunk ID (matches like 20113:"117abc8")
	hashRegex := regexp.MustCompile(chunkID + `\s*:\s*["']([a-f0-9]+)["']`)
	allMatches := hashRegex.FindAllStringSubmatch(html, -1)
	for _, m := range allMatches {
		if len(m) > 1 && m[1] != "ondemand.s" {
			return "https://abs.twimg.com/responsive-web/client-web/ondemand.s." + m[1] + "a.js"
		}
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
