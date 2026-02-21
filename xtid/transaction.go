package xtid

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"math/rand"
	"regexp"
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
}

func newClientTransaction(homePageHTML, ondemandJS string) (*ClientTransaction, error) {
	ct := &ClientTransaction{}

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
	return math.Round(result*100) / 100
}

func (ct *ClientTransaction) animate(frames []int, targetTime float64) string {
	if len(frames) < 11 {
		return ""
	}
	fromColor := []float64{float64(frames[0]), float64(frames[1]), float64(frames[2]), 1}
	toColor := []float64{float64(frames[3]), float64(frames[4]), float64(frames[5]), 1}
	fromRotation := []float64{0.0}
	toRotation := []float64{ct.solve(float64(frames[6]), 60.0, 360.0, true)}

	curveFrames := frames[7:]
	curves := make([]float64, len(curveFrames))
	for i, item := range curveFrames {
		curves[i] = ct.solve(float64(item), isOdd(i), 1.0, false)
	}

	c := newCubic(curves)
	val := c.getValue(targetTime)

	color := interpolate(fromColor, toColor, val)
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
		rounded := math.Round(value*100) / 100
		if rounded < 0 {
			rounded = -rounded
		}
		hexValue := floatToHex(rounded)
		if strings.HasPrefix(hexValue, ".") {
			strArr = append(strArr, "0"+strings.ToLower(hexValue))
		} else if hexValue == "" {
			strArr = append(strArr, "0")
		} else {
			strArr = append(strArr, hexValue)
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

	rowIndex := 0
	if ct.rowIndex < len(ct.keyBytes) {
		rowIndex = int(ct.keyBytes[ct.rowIndex]) % 16
	}

	frameTime := 1.0
	for _, idx := range ct.keyBytesIndices {
		if idx < len(ct.keyBytes) {
			frameTime *= float64(int(ct.keyBytes[idx]) % 16)
		}
	}
	frameTime = jsRound(frameTime/10) * 10

	arr := ct.get2DArray(homePageHTML)
	if arr == nil || rowIndex >= len(arr) {
		return "", fmt.Errorf("failed to get 2D array from SVG frames")
	}

	targetTime := frameTime / totalTime
	return ct.animate(arr[rowIndex], targetTime), nil
}

// GenerateID computes x-client-transaction-id for a given method+path.
func (ct *ClientTransaction) GenerateID(method, path string) string {
	// Strip query string — only path matters
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}

	timeNow := int(time.Now().UnixMilli()-1682924400000) / 1000
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

	randomNum := byte(rand.Intn(256))
	out := make([]byte, len(bytesArr)+1)
	out[0] = randomNum
	for i, b := range bytesArr {
		out[i+1] = b ^ randomNum
	}

	result := base64.StdEncoding.EncodeToString(out)
	return strings.TrimRight(result, "=")
}
