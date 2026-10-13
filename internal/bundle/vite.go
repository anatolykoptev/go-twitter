package bundle

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// Sentinel errors for the Vite-era x.com build.
var (
	// ErrLoggedOutShell: the page is the logged-out Vite shell (its only app
	// script is entry-client-logged-out-*.js). The real bundle is served only to an
	// authenticated session, so the caller needs an account's cookies.
	ErrLoggedOutShell = errors.New("bundle: logged-out x.com web shell (authenticated session required)")
	// ErrIndicesNotFound: no scanned chunk referenced the signing/indices file.
	ErrIndicesNotFound = errors.New("bundle: signing script (sign.o / ondemand.s) not found")
)

const (
	// DefaultScanConcurrency bounds parallel chunk fetches in FindIndicesURL.
	DefaultScanConcurrency = 8
	// DefaultScanMax caps how many chunks FindIndicesURL will fetch.
	DefaultScanMax = 256
	// maxMigrateHops bounds the migration redirect chain in FetchPage.
	maxMigrateHops = 2
)

var (
	// xWebScriptRe matches Vite-era script links: https://<host>/x-web/<path>.js.
	// Hosts are re-checked against the allowlist in ViteScripts.
	xWebScriptRe = regexp.MustCompile(`https://[\w.-]+/x-web/[\w./-]+\.js`)
	// loggedOutEntryRe recognises entry-client-logged-out(-hash).js.
	loggedOutEntryRe = regexp.MustCompile(`(?:^|/)entry-client-logged-out(?:[-.][^/?#]+)?\.js(?:[?#].*)?$`)
	// indicesFileRe finds the indices file reference inside a chunk or URL: the
	// legacy build linked ondemand.s.<hash>a.js, the Vite build dynamically imports
	// sign.o-<hash>.js. \b rejects substring hits such as design.o-*.js.
	indicesFileRe = regexp.MustCompile(`(?:\.{0,2}/)?[\w./-]*?\b(?:ondemand\.s|sign\.o)[\w.-]*\.js`)

	// docLocationRe matches the migrate stub `<script>document.location = "URL"`.
	docLocationRe = regexp.MustCompile(`<script[^>]*>\s*document\.location\s*=\s*"([^"]+)"`)
	migrateForm   = regexp.MustCompile(`<form[^>]*action="https://x\.com/x/migrate"[^>]*>`)
	inputTagRe    = regexp.MustCompile(`<input\b[^>]*>`)
	attrNameRe    = regexp.MustCompile(`\bname="([^"]*)"`)
	attrValueRe   = regexp.MustCompile(`\bvalue="([^"]*)"`)
	jsEscapes     = strings.NewReplacer(`\/`, `/`, `&`, `&`, `\x26`, `&`)
)

// ViteScripts returns the unique x-web script URLs linked in the page, in order,
// restricted to allowlisted hosts (page HTML is third-party data).
func ViteScripts(page string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range xWebScriptRe.FindAllString(page, -1) {
		u, err := url.Parse(m)
		if err != nil || !allowedBundleHost(u.Hostname()) || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// IsLoggedOutShell reports whether any script is the logged-out entry bundle.
func IsLoggedOutShell(scripts []string) bool {
	for _, s := range scripts {
		if loggedOutEntryRe.MatchString(s) {
			return true
		}
	}
	return false
}

// FindIndicesURL locates the indices file among the page's x-web scripts: a
// script URL that names it directly wins; otherwise chunks are fetched with
// bounded concurrency and the first one referencing it is resolved against its
// own URL. Resolved URLs must be https + allowlisted.
func FindIndicesURL(ctx context.Context, f Fetcher, scripts []string, concurrency, maxScan int) (string, error) {
	if concurrency <= 0 {
		concurrency = DefaultScanConcurrency
	}
	if maxScan <= 0 {
		maxScan = DefaultScanMax
	}
	for _, s := range scripts {
		if indicesFileRe.MatchString(s) {
			return s, nil
		}
	}
	if len(scripts) > maxScan {
		scripts = scripts[:maxScan]
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan string)
	found := make(chan string, concurrency)
	var mu sync.Mutex
	loaded, failed := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				body, err := f.Fetch(ctx, u)
				mu.Lock()
				if err != nil {
					failed++
				} else {
					loaded++
				}
				mu.Unlock()
				if err != nil {
					continue
				}
				if hit, ok := resolveIndicesRef(u, body); ok {
					found <- hit
					cancel()
					return
				}
			}
		}()
	}
feed:
	for _, s := range scripts {
		select {
		case jobs <- s:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	close(found)

	if hit, ok := <-found; ok {
		return hit, nil
	}
	mu.Lock()
	defer mu.Unlock()
	return "", fmt.Errorf("%w (chunks: %d loaded, %d failed)", ErrIndicesNotFound, loaded, failed)
}

// resolveIndicesRef finds the indices-file reference in a chunk body and
// resolves it against the chunk URL.
func resolveIndicesRef(chunkURL string, body []byte) (string, bool) {
	ref := indicesFileRe.Find(body)
	if ref == nil {
		return "", false
	}
	base, err := url.Parse(chunkURL)
	if err != nil {
		return "", false
	}
	rel, err := url.Parse(string(ref))
	if err != nil {
		return "", false
	}
	abs := base.ResolveReference(rel)
	if abs.Scheme != "https" || !allowedBundleHost(abs.Hostname()) {
		return "", false
	}
	return abs.String(), true
}

// FetchPage fetches pageURL and follows the migration hops some sessions get: a
// stub `<script>document.location = "URL"` page, then an x.com/x/migrate
// auto-submit form. The form POST needs a Poster fetcher; without one a page
// that demands it is an error, not a silent pass-through of the stub.
func FetchPage(ctx context.Context, f Fetcher, pageURL string) ([]byte, error) {
	body, err := f.Fetch(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	for hop := 0; hop < maxMigrateHops; hop++ {
		if m := docLocationRe.FindSubmatch(body); m != nil {
			next := jsEscapes.Replace(string(m[1]))
			if !safePageURL(next) {
				return nil, fmt.Errorf("bundle: refusing document.location redirect to %q", next)
			}
			if body, err = f.Fetch(ctx, next); err != nil {
				return nil, err
			}
			continue
		}
		if form, ok := parseMigrateForm(body); ok {
			p, canPost := f.(Poster)
			if !canPost {
				return nil, errors.New("bundle: page requires the x.com/x/migrate form post but the fetcher cannot POST")
			}
			if body, err = p.PostForm(ctx, "https://x.com/x/migrate", form); err != nil {
				return nil, err
			}
			continue
		}
		break
	}
	return body, nil
}

func safePageURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && allowedBundleHost(u.Hostname())
}

// parseMigrateForm extracts the inputs of the x.com/x/migrate form.
func parseMigrateForm(body []byte) (url.Values, bool) {
	if !migrateForm.Match(body) {
		return nil, false
	}
	form := url.Values{}
	for _, tag := range inputTagRe.FindAll(body, -1) {
		n := attrNameRe.FindSubmatch(tag)
		if n == nil {
			continue
		}
		val := ""
		if v := attrValueRe.FindSubmatch(tag); v != nil {
			val = html.UnescapeString(string(v[1]))
		}
		form.Set(html.UnescapeString(string(n[1])), val)
	}
	return form, true
}
