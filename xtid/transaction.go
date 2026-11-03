package xtid

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ClientTransaction generates x-client-transaction-id headers for Twitter/X API requests.
// Algorithm reverse-engineered from Twitter's web app:
// - https://github.com/iSarabjitDhiman/XClientTransaction (Python original, MIT)
// - https://github.com/Ben-Lazrek-Yassine/X-transaction-id-generator-go-rewrite (Go port, MIT)
// - https://antibot.blog/posts/1741552025433 (analysis)
type ClientTransaction struct {
	keyBytes        []byte
	animationKey    string
	rowIndex        int
	keyBytesIndices []int
	// now and randByte are injectable seams (default time.Now / rand) so a fixed
	// seed makes GenerateID reproducible in golden tests. Production is unchanged.
	now      func() time.Time
	randByte func() byte
}

func newClientTransaction(homePageHTML, ondemandJS string) (*ClientTransaction, error) {
	ct := &ClientTransaction{
		now:      time.Now,
		randByte: func() byte { return byte(rand.Intn(256)) },
	}

	rowIndex, keyIndices := getKeyIndices(ondemandJS)
	ct.rowIndex = rowIndex
	ct.keyBytesIndices = keyIndices

	key := getVerificationKey(homePageHTML)
	if key == "" {
		return nil, fmt.Errorf("twitter-site-verification meta tag not found")
	}
	keyBytes, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("decode verification key: %w", err)
	}
	ct.keyBytes = keyBytes

	animKey, err := ct.buildAnimationKey(homePageHTML)
	if err != nil {
		return nil, fmt.Errorf("build animation key: %w", err)
	}
	ct.animationKey = animKey

	return ct, nil
}

func (ct *ClientTransaction) get2DArray(homePageHTML string) [][]int {
	frames := getSVGFrames(homePageHTML)
	if len(frames) == 0 || len(ct.keyBytes) < 6 {
		return nil
	}
	frameIndex := int(ct.keyBytes[5]) % 4
	if frameIndex >= len(frames) || len(frames[frameIndex].data) == 0 {
		return nil
	}
	return frames[frameIndex].data
}

func (ct *ClientTransaction) solve(value float64, minVal, maxVal float64, rounding bool) float64 {
	result := value*(maxVal-minVal)/255 + minVal
	if rounding {
		return math.Floor(result)
	}
	return round2(result)
}

// round2 is the reference's `Math.round(v*100)/100`. JS rounds a tie toward
// +Inf, math.Round away from zero: they differ for negative ties (-0.125*100 is
// exactly -12.5). The matrix values are made positive only after this rounding,
// so the tie direction decides the digit.
func round2(v float64) float64 {
	// The explicit conversion keeps v*100 rounded before jsRound subtracts the
	// floor: a fused multiply-subtract would see the unrounded product and turn
	// an exact tie (-299.5) into 0.4999... on arm64.
	return jsRound(float64(v*100)) / 100
}

// matrixNumberRe is the pattern X's script uses to pull the numbers out of the
// computed `matrix(a, b, c, d, 0, 0)` string.
var matrixNumberRe = regexp.MustCompile(`[\d.-]+`)

// matrixNumbers returns the number tokens X's script reads from one matrix
// entry. X reads the entry from getComputedStyle, where Chromium prints it with
// six significant digits ("0.719009", and "5.68575e-05" for small values), then
// matches /[\d.-]+/g over that string: the exponent of "5.68575e-05" therefore
// comes out as a second number, "-05". Both effects change the key: 0.69499999
// reads as "0.695", and a value below 1e-4 yields two numbers.
func matrixNumbers(v float64) []float64 {
	var out []float64
	for _, tok := range matrixNumberRe.FindAllString(strconv.FormatFloat(v, 'g', 6, 64), -1) {
		if n, err := strconv.ParseFloat(tok, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// toFixed2 is JavaScript's Number(x.toFixed(2)): the decimal expansion of the
// double is rounded to two places, and an exact tie goes to the larger
// magnitude where Go's formatting rounds a tie to even. Only odd multiples of
// 1/8 tie; their expansion has exactly three decimals ("0.125"), so a tie is
// rounded up by incrementing the two-decimal prefix, exactly at any magnitude.
// A zero result is returned as +0: JavaScript keeps the sign of a small
// negative ("-0"), but both print as "0".
func toFixed2(x float64) float64 {
	ax := math.Abs(x)
	s := strconv.FormatFloat(ax, 'f', 2, 64)
	if e := ax * 8; e == math.Floor(e) && math.Mod(e, 2) == 1 {
		s3 := strconv.FormatFloat(ax, 'f', 3, 64)
		s = incrementDecimal(s3[:len(s3)-1])
	}
	v, _ := strconv.ParseFloat(s, 64)
	if v == 0 {
		return 0
	}
	return math.Copysign(v, x)
}

// incrementDecimal adds one unit in the last place to a non-negative decimal
// string such as "12.99".
func incrementDecimal(s string) string {
	b := []byte(s)
	for i := len(b) - 1; i >= 0; i-- {
		switch b[i] {
		case '.':
			continue
		case '9':
			b[i] = '0'
		default:
			b[i]++
			return string(b)
		}
	}
	return "1" + string(b)
}

func (ct *ClientTransaction) animate(frames []int, targetTime float64) (string, error) {
	// Colours (6), rotation (1) and the four control values of the easing curve.
	if len(frames) < 11 {
		return "", fmt.Errorf("animation row has %d numbers, want at least 11", len(frames))
	}
	curveFrames := frames[7:]
	curves := make([]float64, len(curveFrames))
	for i, item := range curveFrames {
		curves[i] = ct.solve(float64(item), isOdd(i), 1.0, false)
	}

	return ct.render(frames, newCubic(curves).getValue(targetTime)), nil
}

// render prints the animation key for the eased value val: the colour and the
// rotation matrix X reads back from the browser after setting the animation to
// that point.
func (ct *ClientTransaction) render(frames []int, val float64) string {
	fromColor := []float64{float64(frames[0]), float64(frames[1]), float64(frames[2]), 1}
	toColor := []float64{float64(frames[3]), float64(frames[4]), float64(frames[5]), 1}
	fromRotation := []float64{0.0}
	toRotation := []float64{ct.solve(float64(frames[6]), 60.0, 360.0, true)}
	color := interpolate(fromColor, toColor, val)
	// Cap every channel to 0..255, as the browser does when it serialises the
	// computed colour ("rgb(152, 2, 33)"). The eased value overshoots [0,1] (the
	// curve's y control values lie in [-1,1]), so a raw channel can reach ~300 in
	// about 5% of verification keys (219 of 4001 on the live page). Printed
	// unclamped it is three hex digits ("125" instead of "ff") and X answers 404
	// to the id. The Lqm1 reference floors at 0 but does not cap, so it cannot
	// tell: testdata/browser holds the browser's own answers.
	for i := range color {
		color[i] = math.Max(0, math.Min(255, color[i]))
	}

	rotation := interpolate(fromRotation, toRotation, val)
	matrix := convertRotationToMatrix(rotation[0])

	var strArr []string
	for i := 0; i < 3; i++ {
		strArr = append(strArr, fmt.Sprintf("%x", int(math.Round(color[i]))))
	}
	for _, value := range matrix {
		for _, token := range matrixNumbers(value) {
			hexValue := floatToHex(math.Abs(toFixed2(token)))
			if strings.HasPrefix(hexValue, ".") {
				strArr = append(strArr, "0"+strings.ToLower(hexValue))
			} else if hexValue == "" {
				strArr = append(strArr, "0")
			} else {
				strArr = append(strArr, hexValue)
			}
		}
	}
	strArr = append(strArr, "0", "0")

	result := strings.Join(strArr, "")
	result = regexp.MustCompile(`[.-]`).ReplaceAllString(result, "")
	return result
}

func (ct *ClientTransaction) buildAnimationKey(homePageHTML string) (string, error) {
	const totalTime = 4096.0

	if len(ct.keyBytesIndices) == 0 {
		return "", fmt.Errorf("no key byte indices")
	}

	// An index outside the key means the page and the script do not belong
	// together (or the key is truncated). Building from a different byte than X
	// would give a well-formed id that X rejects, silently, for the whole life of
	// the keys; failing here makes the manager keep its previous keys and retry.
	if ct.rowIndex >= len(ct.keyBytes) {
		return "", fmt.Errorf("row index %d outside the %d-byte verification key", ct.rowIndex, len(ct.keyBytes))
	}
	rowIndex := int(ct.keyBytes[ct.rowIndex]) % 16

	frameTime := 1.0
	for _, idx := range ct.keyBytesIndices {
		if idx >= len(ct.keyBytes) {
			return "", fmt.Errorf("key byte index %d outside the %d-byte verification key", idx, len(ct.keyBytes))
		}
		frameTime *= float64(int(ct.keyBytes[idx]) % 16)
	}
	frameTime = jsRound(frameTime/10) * 10

	arr := ct.get2DArray(homePageHTML)
	if arr == nil || rowIndex >= len(arr) {
		return "", fmt.Errorf("failed to get 2D array from SVG frames")
	}

	targetTime := frameTime / totalTime
	return ct.animate(arr[rowIndex], targetTime)
}

// nowFn returns the injected clock, defaulting to time.Now so a zero-value
// ClientTransaction (not built via newClientTransaction) is still safe.
func (ct *ClientTransaction) nowFn() time.Time {
	if ct.now != nil {
		return ct.now()
	}
	return time.Now()
}

// randByteFn returns the injected random source, defaulting to rand.
func (ct *ClientTransaction) randByteFn() byte {
	if ct.randByte != nil {
		return ct.randByte()
	}
	return byte(rand.Intn(256))
}

// GenerateID computes x-client-transaction-id for a given method+path.
func (ct *ClientTransaction) GenerateID(method, path string) string {
	// Strip the query string: only the path is signed. The reference signs
	// whatever string it is given, so the query-free path matches its output for
	// the same path. A live probe returned 200 for UserTweetsAndReplies with the
	// variables/features in the query while the id was computed on the bare path.
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}

	timeNow := int(ct.nowFn().UnixMilli()-1682924400000) / 1000
	timeNowBytes := make([]byte, 4)
	for i := 0; i < 4; i++ {
		timeNowBytes[i] = byte((timeNow >> (i * 8)) & 0xFF)
	}

	hashInput := fmt.Sprintf("%s!%s!%d%s%s", method, path, timeNow, defaultKeyword, ct.animationKey)
	hash := sha256.Sum256([]byte(hashInput))
	hashBytes := hash[:16]

	bytesArr := make([]byte, 0, len(ct.keyBytes)+4+16+1)
	bytesArr = append(bytesArr, ct.keyBytes...)
	bytesArr = append(bytesArr, timeNowBytes...)
	bytesArr = append(bytesArr, hashBytes...)
	bytesArr = append(bytesArr, byte(additionalRandomNumber))

	randomNum := ct.randByteFn()
	out := make([]byte, len(bytesArr)+1)
	out[0] = randomNum
	for i, b := range bytesArr {
		out[i+1] = b ^ randomNum
	}

	result := base64.StdEncoding.EncodeToString(out)
	return strings.TrimRight(result, "=")
}
