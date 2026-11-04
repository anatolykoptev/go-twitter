package twitter

import (
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
)

// wireOrder is the order the transport's HTTP/2 encoder writes a request's
// headers in, computed with the transport's own sorter (fhttp, the library
// under tls-client) rather than a re-implementation: the headers named in order
// first, in that order, then every other header in lexicographic order. The
// two headers the encoder adds itself are included the way it adds them:
// content-length for a POST, and accept-encoding when the caller sent none.
func wireOrder(method string, headers map[string]string, order []string) []string {
	h := fhttp.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	hasAE := false
	for k := range headers {
		if strings.EqualFold(k, "accept-encoding") {
			hasAE = true
		}
	}
	if !hasAE {
		h["accept-encoding"] = []string{"gzip, deflate, br, zstd"}
	}
	if method == "POST" {
		h["content-length"] = []string{"0"}
	}
	byName := make(map[string]int, len(order))
	for i, n := range order {
		byName[n] = i
	}
	kvs, _ := h.SortedKeyValuesBy(byName, map[string]bool{})
	out := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, strings.ToLower(kv.Key))
	}
	return out
}
