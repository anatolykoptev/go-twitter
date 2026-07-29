package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
)

const (
	// mediaUploadURL is Twitter's v1.1 chunked media upload endpoint (NOT GraphQL).
	mediaUploadURL = "https://upload.twitter.com/1.1/media/upload.json"

	// mediaChunkSize is the per-APPEND segment size (~1 MiB).
	mediaChunkSize = 1 << 20

	// maxMediaPollAttempts bounds the FINALIZE/STATUS polling loop for async
	// (video/gif) processing so a stuck "in_progress" never blocks forever.
	maxMediaPollAttempts = 60

	// defaultMediaPollInterval is used when Twitter does not supply check_after_secs.
	defaultMediaPollInterval = 1 * time.Second

	// maxMediaPollWait clamps the per-poll wait so a hostile or buggy
	// server-supplied check_after_secs cannot pin the call for a long time.
	maxMediaPollWait = 30 * time.Second

	// epUploadMedia is the metrics/rate-limit endpoint label for media uploads.
	epUploadMedia = "UploadMedia"
)

// processingInfo mirrors Twitter's async media processing state returned by
// FINALIZE and STATUS. Images return no processing_info; video/gif do.
type processingInfo struct {
	State      string `json:"state"`
	CheckAfter int    `json:"check_after_secs"`
	Progress   int    `json:"progress_percent"`
	Error      *struct {
		Code    int    `json:"code"`
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"error"`
}

// uploadDoFunc performs a single media-endpoint request and returns the raw
// response body. It is injected so the upload state machine can be tested
// without the stealth client or live Twitter.
type uploadDoFunc func(ctx context.Context, method, urlStr string, body []byte, contentType string) ([]byte, error)

// UploadMedia uploads raw media bytes via the chunked INIT/APPEND/FINALIZE flow
// using the given account's authenticated stealth client, and returns the
// media_id string for use in CreateTweetWithMedia. mediaType is the MIME type
// (e.g. "image/jpeg", "image/gif", "video/mp4").
func (c *Client) UploadMedia(ctx context.Context, acc *Account, data []byte, mediaType string) (string, error) {
	if acc == nil {
		return "", fmt.Errorf("UploadMedia: nil account")
	}
	do := func(ctx context.Context, method, urlStr string, body []byte, contentType string) ([]byte, error) {
		return c.doMediaRequest(ctx, acc, method, urlStr, body, contentType)
	}
	sleep := func(ctx context.Context, d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return runMediaUpload(ctx, do, sleep, mediaUploadURL, data, mediaType, mediaChunkSize)
}

// UploadMediaFile reads a file from disk, infers its MIME type, and uploads it.
func (c *Client) UploadMediaFile(ctx context.Context, acc *Account, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("UploadMediaFile: read %s: %w", path, err)
	}
	return c.UploadMedia(ctx, acc, data, mediaTypeForFile(path, data))
}

// runMediaUpload drives the chunked upload state machine:
// INIT -> APPEND (per segment) -> FINALIZE -> STATUS poll (async media only).
func runMediaUpload(ctx context.Context, do uploadDoFunc, sleep func(context.Context, time.Duration) error, uploadURL string, data []byte, mediaType string, chunkSize int) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("media upload: empty data")
	}
	if chunkSize <= 0 {
		chunkSize = mediaChunkSize
	}
	category, err := inferMediaCategory(mediaType)
	if err != nil {
		return "", err
	}

	// INIT
	initQ := url.Values{}
	initQ.Set("command", "INIT")
	initQ.Set("total_bytes", strconv.Itoa(len(data)))
	initQ.Set("media_type", mediaType)
	initQ.Set("media_category", category)
	initBody, err := do(ctx, http.MethodPost, uploadURL+"?"+initQ.Encode(), nil, "")
	if err != nil {
		return "", fmt.Errorf("media INIT: %w", err)
	}
	mediaID, err := parseMediaInit(initBody)
	if err != nil {
		return "", fmt.Errorf("media INIT: %w", err)
	}

	// APPEND — one segment per chunk, sequential segment_index.
	for i, r := range chunkRanges(len(data), chunkSize) {
		appBody, contentType, err := buildAppendMultipart(data[r[0]:r[1]])
		if err != nil {
			return "", fmt.Errorf("media APPEND segment %d: %w", i, err)
		}
		appQ := url.Values{}
		appQ.Set("command", "APPEND")
		appQ.Set("media_id", mediaID)
		appQ.Set("segment_index", strconv.Itoa(i))
		if _, err := do(ctx, http.MethodPost, uploadURL+"?"+appQ.Encode(), appBody, contentType); err != nil {
			return "", fmt.Errorf("media APPEND segment %d: %w", i, err)
		}
	}

	// FINALIZE
	finQ := url.Values{}
	finQ.Set("command", "FINALIZE")
	finQ.Set("media_id", mediaID)
	finQ.Set("allow_async", "true")
	finBody, err := do(ctx, http.MethodPost, uploadURL+"?"+finQ.Encode(), nil, "")
	if err != nil {
		return "", fmt.Errorf("media FINALIZE: %w", err)
	}
	info, err := parseProcessingInfo(finBody)
	if err != nil {
		return "", fmt.Errorf("media FINALIZE: %w", err)
	}

	// STATUS poll — images return no processing_info and exit immediately;
	// video/gif report pending/in_progress until succeeded or failed.
	for attempt := 0; ; attempt++ {
		done, err := processingDone(info)
		if err != nil {
			return "", err
		}
		if done {
			return mediaID, nil
		}
		if attempt >= maxMediaPollAttempts {
			return "", fmt.Errorf("media processing: timeout after %d polls (last state %q)", attempt, info.State)
		}
		wait := time.Duration(info.CheckAfter) * time.Second
		if wait <= 0 {
			wait = defaultMediaPollInterval
		}
		if wait > maxMediaPollWait {
			wait = maxMediaPollWait
		}
		if err := sleep(ctx, wait); err != nil {
			return "", err
		}

		stQ := url.Values{}
		stQ.Set("command", "STATUS")
		stQ.Set("media_id", mediaID)
		stBody, err := do(ctx, http.MethodGet, uploadURL+"?"+stQ.Encode(), nil, "")
		if err != nil {
			return "", fmt.Errorf("media STATUS: %w", err)
		}
		info, err = parseProcessingInfo(stBody)
		if err != nil {
			return "", fmt.Errorf("media STATUS: %w", err)
		}
	}
}

// inferMediaCategory maps a MIME type to Twitter's media_category.
func inferMediaCategory(mediaType string) (string, error) {
	switch {
	case mediaType == "image/gif":
		return "tweet_gif", nil
	case strings.HasPrefix(mediaType, "image/"):
		return "tweet_image", nil
	case strings.HasPrefix(mediaType, "video/"):
		return "tweet_video", nil
	default:
		return "", fmt.Errorf("unsupported media type %q (want image/* or video/*)", mediaType)
	}
}

// chunkRanges splits total bytes into [start,end) segment ranges of at most
// chunkSize. The slice index doubles as the APPEND segment_index.
func chunkRanges(total, chunkSize int) [][2]int {
	if total <= 0 || chunkSize <= 0 {
		return nil
	}
	var ranges [][2]int
	for start := 0; start < total; start += chunkSize {
		end := start + chunkSize
		if end > total {
			end = total
		}
		ranges = append(ranges, [2]int{start, end})
	}
	return ranges
}

// buildAppendMultipart builds the multipart/form-data body for an APPEND
// request with the chunk under form field "media", returning the body and its
// content type (which carries the boundary).
func buildAppendMultipart(chunk []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("media", "blob")
	if err != nil {
		return nil, "", fmt.Errorf("create multipart media field: %w", err)
	}
	if _, err := fw.Write(chunk); err != nil {
		return nil, "", fmt.Errorf("write multipart chunk: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// parseMediaInit extracts media_id_string from an INIT response.
func parseMediaInit(body []byte) (string, error) {
	var raw struct {
		MediaIDString string          `json:"media_id_string"`
		Errors        []mediaAPIError `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("unmarshal media INIT: %w", err)
	}
	if len(raw.Errors) > 0 {
		return "", fmt.Errorf("media INIT API error %d: %s", raw.Errors[0].Code, raw.Errors[0].Message)
	}
	if raw.MediaIDString == "" {
		return "", fmt.Errorf("media INIT returned empty media_id: %s", truncateBytes(body, 200))
	}
	return raw.MediaIDString, nil
}

// parseProcessingInfo extracts processing_info from a FINALIZE/STATUS response.
// Returns (nil, nil) when no processing_info is present (e.g. images, or 204).
func parseProcessingInfo(body []byte) (*processingInfo, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var raw struct {
		ProcessingInfo *processingInfo `json:"processing_info"`
		Errors         []mediaAPIError `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal processing_info: %w", err)
	}
	if len(raw.Errors) > 0 {
		return nil, fmt.Errorf("media API error %d: %s", raw.Errors[0].Code, raw.Errors[0].Message)
	}
	return raw.ProcessingInfo, nil
}

// mediaAPIError is the shared shape of a Twitter media-endpoint error entry.
type mediaAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// processingDone reports whether async media processing is complete. A nil
// processingInfo (image, or absent) counts as done. A "failed" state is an error.
func processingDone(p *processingInfo) (bool, error) {
	if p == nil {
		return true, nil
	}
	switch p.State {
	case "succeeded", "":
		return true, nil
	case "failed":
		if p.Error != nil {
			return false, fmt.Errorf("media processing failed: %s (%s)", p.Error.Message, p.Error.Name)
		}
		return false, fmt.Errorf("media processing failed (progress %d%%)", p.Progress)
	case "pending", "in_progress":
		return false, nil
	default:
		// Unknown state — keep polling; the attempt counter enforces a timeout.
		return false, nil
	}
}

// mediaTypeForFile infers a MIME type from the file extension, falling back to
// content sniffing when the extension is unknown.
func mediaTypeForFile(path string, data []byte) string {
	if ext := strings.ToLower(filepath.Ext(path)); ext != "" {
		if mt := mime.TypeByExtension(ext); mt != "" {
			if i := strings.IndexByte(mt, ';'); i >= 0 {
				mt = strings.TrimSpace(mt[:i])
			}
			return mt
		}
	}
	return http.DetectContentType(data)
}

// CreateTweetWithMedia posts a tweet with attached media IDs from a specific
// account. With no media IDs it is equivalent to CreateTweet. Returns the
// tweet ID on success.
func (c *Client) CreateTweetWithMedia(ctx context.Context, acc *Account, text string, mediaIDs []string) (string, error) {
	ep := Endpoints["CreateTweet"]
	payload, err := json.Marshal(map[string]any{
		"variables": createTweetVariables(text, mediaIDs),
		"features":  ep.Features,
		"queryId":   ep.ID,
	})
	if err != nil {
		return "", fmt.Errorf("marshal CreateTweetWithMedia payload: %w", err)
	}

	body, err := c.doPOST(ctx, acc, "CreateTweet", ep.URL(), payload)
	if err != nil {
		return "", fmt.Errorf("CreateTweetWithMedia: %w", err)
	}
	return parseCreateTweet(body)
}

// PostWithMedia uploads the given files and posts a tweet attaching them, from a
// named account (by username). Returns the tweet ID on success.
func (c *Client) PostWithMedia(ctx context.Context, username, text string, paths ...string) (string, error) {
	acc := c.AccountByUsername(username)
	if acc == nil {
		return "", fmt.Errorf("account %q not found in pool", username)
	}
	if !acc.IsActive() {
		return "", fmt.Errorf("account %q is not active", username)
	}

	mediaIDs := make([]string, 0, len(paths))
	for _, p := range paths {
		id, err := c.UploadMediaFile(ctx, acc, p)
		if err != nil {
			return "", fmt.Errorf("PostWithMedia: upload %s: %w", p, err)
		}
		mediaIDs = append(mediaIDs, id)
	}
	return c.CreateTweetWithMedia(ctx, acc, text, mediaIDs)
}

// createTweetVariables builds the CreateTweet GraphQL variables, populating
// media.media_entities from the given media IDs.
func createTweetVariables(text string, mediaIDs []string) map[string]any {
	entities := make([]any, 0, len(mediaIDs))
	for _, id := range mediaIDs {
		entities = append(entities, map[string]any{
			"media_id":     id,
			"tagged_users": []any{},
		})
	}
	return map[string]any{
		"tweet_text":   text,
		"dark_request": false,
		"media": map[string]any{
			"media_entities":     entities,
			"possibly_sensitive": false,
		},
		"semantic_annotation_ids": []any{},
	}
}

// doMediaRequest executes an authenticated request to the media upload host
// using the given account's stealth client, mirroring doPOST's CSRF rotation,
// relogin, and error-classification handling but for multipart/query uploads.
// Accepts 200/201/204 as success.
func (c *Client) doMediaRequest(ctx context.Context, acc *Account, method, urlStr string, payload []byte, contentType string) ([]byte, error) {
	// NOTE: media uploads are a low-frequency POST path, NOT the scrape burst
	// source, so they are intentionally NOT routed through the per-account human
	// pacer wired at doPoolRequest (see buildAccountPacer SCOPE note). Only the
	// anti-fingerprint jitter applies here. If this path ever becomes bursty,
	// call c.accountPacer.Wait(ctx, acc.ID()) after the account is selected.
	if err := stealth.DefaultJitter.Sleep(ctx); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			delay := stealth.DefaultBackoff.Duration(attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// NO proactive ct0 rotation (see doPoolRequest / shouldProactivelyRotate):
		// client-rotating an established session's ct0 forces CSRF-353. ct0 stays
		// current via the server's set-cookie adopted on each success below.
		bc := c.clientForAccount(acc)
		authTok, ct0, ua := acc.Credentials()
		body, respHdrs, status, err := c.doPoolReq(bc, method, urlStr, payload, mediaHeaders(authTok, ct0, ua, contentType))
		if err != nil {
			c.attributeTransportError(acc, err)
			lastErr = err
			continue
		}

		// Reset proxy consecutive failures on any HTTP response.
		acc.mu.Lock()
		acc.proxyConsecFails = 0
		acc.mu.Unlock()

		switch {
		case status == 429:
			c.recordAPICall(epUploadMedia, false, true)
			acc.MarkEndpointRateLimited(epUploadMedia, parseRateLimitReset(respHdrs["x-rate-limit-reset"]))
			lastErr = &APIError{Status: status, Class: errRateLimited, Message: "429 rate limited"}
			continue

		case status == 401 || status == 403:
			c.recordAPICall(epUploadMedia, false, false)
			switch classifyError(body, respHdrs) {
			case errCSRF:
				if body2, ok := c.recoverCSRFMedia(acc, bc, method, urlStr, payload, contentType, respHdrs, &lastErr); ok {
					return body2, nil
				}
				continue
			case errAuthExpired:
				slog.Warn("UploadMedia: auth expired, attempting relogin", slog.String("user", acc.Username))
				if reErr := c.relogin(acc); reErr != nil {
					lastErr = fmt.Errorf("relogin failed: %w", reErr)
					continue
				}
				authTok2, ct02, ua2 := acc.Credentials()
				body2, respHdrs2, status2, err2 := c.doPoolReq(bc, method, urlStr, payload, mediaHeaders(authTok2, ct02, ua2, contentType))
				if err2 == nil && isUploadOK(status2) {
					c.recordAPICall(epUploadMedia, true, false)
					acc.RecordSuccess()
					return body2, nil
				}
				apiErr := newAPIError(status2, body2, respHdrs2)
				apiErr.Message = "post-relogin upload failed"
				lastErr = apiErr
				continue
			default:
				acc.RecordFailure()
				apiErr := newAPIError(status, body, respHdrs)
				apiErr.Message = fmt.Sprintf("%s HTTP %d: %s", epUploadMedia, status, truncateBytes(body, 200))
				return nil, apiErr
			}

		case !isUploadOK(status):
			c.recordAPICall(epUploadMedia, false, false)
			acc.RecordFailure()
			apiErr := newAPIError(status, body, respHdrs)
			apiErr.Message = fmt.Sprintf("%s HTTP %d: %s", epUploadMedia, status, truncateBytes(body, 200))
			return nil, apiErr
		}

		// 2xx — check the body for Twitter error codes.
		if cls := classifyError(body, respHdrs); cls != errNone {
			c.recordAPICall(epUploadMedia, false, false)
			acc.RecordFailure()
			apiErr := newAPIError(200, body, respHdrs)
			apiErr.Message = fmt.Sprintf("%s error class %d: %s", epUploadMedia, cls, truncateBytes(body, 200))
			return nil, apiErr
		}
		if newCT0 := extractCT0FromHeaders(respHdrs); newCT0 != "" && newCT0 != ct0 {
			acc.SetCT0(newCT0)
			authTok2, ct02, _ := acc.Credentials()
			_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
		}
		c.recordAPICall(epUploadMedia, true, false)
		acc.RecordSuccess()
		return body, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("%s failed after %d attempts: %w", epUploadMedia, maxRetries, lastErr)
	}
	return nil, fmt.Errorf("%s failed after %d attempts", epUploadMedia, maxRetries)
}

// isUploadOK reports whether an HTTP status counts as a successful upload step.
func isUploadOK(status int) bool {
	return status == http.StatusOK || status == http.StatusCreated || status == http.StatusNoContent
}

// recoverCSRFMedia is the media-upload (doMediaRequest) twin of recoverCSRF. It
// adopts the SERVER ct0 from the 353 response and retries; only if x.com offered
// no ct0 to adopt (or the adopted ct0 still fails) does it relogin and retry. It
// never generates a client-random ct0. Returns (body, true) on a recovered
// success; (nil, false) when recovery failed — the caller sets lastErr and
// `continue`s.
func (c *Client) recoverCSRFMedia(acc *Account, bc *stealth.BrowserClient, method, urlStr string, payload []byte, contentType string, respHdrs map[string]string, lastErr *error) ([]byte, bool) {
	if serverCT0, ok := csrfRecoveryCT0(respHdrs); ok {
		slog.Warn("UploadMedia: CSRF error 353, adopting server ct0", slog.String("user", acc.Username))
		acc.SetCT0(serverCT0)
		authTok2, ct02, ua2 := acc.Credentials()
		_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
		body2, respHdrs2, status2, err2 := c.doPoolReq(bc, method, urlStr, payload, mediaHeaders(authTok2, ct02, ua2, contentType))
		if err2 == nil && isUploadOK(status2) && classifyError(body2, respHdrs2) == errNone {
			if newCT0 := extractCT0FromHeaders(respHdrs2); newCT0 != "" {
				acc.SetCT0(newCT0)
				authTok3, ct03, _ := acc.Credentials()
				_ = saveSession(c.cfg.SessionDir, acc.Username, authTok3, ct03)
			}
			c.recordAPICall(epUploadMedia, true, false)
			acc.RecordSuccess()
			return body2, true
		}
		acc.RecordFailure()
		slog.Warn("UploadMedia: CSRF retry with server ct0 failed, attempting relogin", slog.String("user", acc.Username))
	} else {
		slog.Warn("UploadMedia: CSRF error 353, no server ct0 to adopt, attempting relogin", slog.String("user", acc.Username))
	}

	if reErr := c.relogin(acc); reErr != nil {
		slog.Warn("UploadMedia: relogin after CSRF failed", slog.String("user", acc.Username), slog.Any("error", reErr))
		*lastErr = fmt.Errorf("relogin failed: %w", reErr)
		return nil, false
	}
	authTok3, ct03, ua3 := acc.Credentials()
	body3, respHdrs3, status3, err3 := c.doPoolReq(bc, method, urlStr, payload, mediaHeaders(authTok3, ct03, ua3, contentType))
	if err3 == nil && isUploadOK(status3) && classifyError(body3, respHdrs3) == errNone {
		c.recordAPICall(epUploadMedia, true, false)
		acc.RecordSuccess()
		return body3, true
	}
	apiErr := newAPIError(status3, body3, respHdrs3)
	apiErr.Message = "post-relogin upload failed"
	*lastErr = apiErr
	return nil, false
}

// mediaHeaders returns the authenticated headers for a media upload request.
// When contentType is empty (bodyless INIT/FINALIZE/STATUS) the JSON content
// type is dropped; for APPEND it is set to the multipart boundary type.
func mediaHeaders(authToken, ct0, userAgent, contentType string) map[string]string {
	h := twitterHeaders(authToken, ct0, userAgent)
	if contentType != "" {
		h["content-type"] = contentType
	} else {
		delete(h, "content-type")
	}
	return h
}
