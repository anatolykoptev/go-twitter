package twitter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInferMediaCategory(t *testing.T) {
	tests := []struct {
		mediaType string
		want      string
		wantErr   bool
	}{
		{"image/jpeg", "tweet_image", false},
		{"image/png", "tweet_image", false},
		{"image/webp", "tweet_image", false},
		{"image/gif", "tweet_gif", false},
		{"video/mp4", "tweet_video", false},
		{"video/quicktime", "tweet_video", false},
		{"application/pdf", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := inferMediaCategory(tt.mediaType)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("inferMediaCategory(%q): expected error", tt.mediaType)
			}
			continue
		}
		if err != nil {
			t.Fatalf("inferMediaCategory(%q): unexpected error: %v", tt.mediaType, err)
		}
		if got != tt.want {
			t.Fatalf("inferMediaCategory(%q) = %q, want %q", tt.mediaType, got, tt.want)
		}
	}
}

func TestChunkRanges(t *testing.T) {
	// 2.5M into 1M chunks -> [0,1M],[1M,2M],[2M,2.5M]
	ranges := chunkRanges(2_500_000, 1_000_000)
	if len(ranges) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(ranges))
	}
	if ranges[2][1]-ranges[2][0] != 500_000 {
		t.Fatalf("expected last chunk 500000 bytes, got %d", ranges[2][1]-ranges[2][0])
	}

	// Exact multiple -> no trailing empty segment.
	if got := len(chunkRanges(2_000_000, 1_000_000)); got != 2 {
		t.Fatalf("exact multiple: expected 2 segments, got %d", got)
	}

	// Smaller than one chunk -> single segment.
	single := chunkRanges(500, 1_000_000)
	if len(single) != 1 || single[0] != [2]int{0, 500} {
		t.Fatalf("sub-chunk: expected [[0 500]], got %v", single)
	}

	// Empty -> no segments.
	if got := chunkRanges(0, 1_000_000); got != nil {
		t.Fatalf("empty: expected nil, got %v", got)
	}

	// Segment boundaries must be contiguous and cover the whole range.
	data := bytes.Repeat([]byte("x"), 3333)
	var reassembled []byte
	for i, r := range chunkRanges(len(data), 1000) {
		if r[0] != i*1000 {
			t.Fatalf("segment %d starts at %d, want %d", i, r[0], i*1000)
		}
		reassembled = append(reassembled, data[r[0]:r[1]]...)
	}
	if !bytes.Equal(reassembled, data) {
		t.Fatal("reassembled chunks != original data")
	}
}

func TestBuildAppendMultipart(t *testing.T) {
	chunk := []byte("hello-multipart-bytes")
	body, contentType, err := buildAppendMultipart(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("expected multipart content-type, got %q", contentType)
	}

	_, params, err := parseMediaContentType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	part, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if part.FormName() != "media" {
		t.Fatalf("expected form field name 'media', got %q", part.FormName())
	}
	got, _ := io.ReadAll(part)
	if !bytes.Equal(got, chunk) {
		t.Fatalf("multipart payload = %q, want %q", got, chunk)
	}
}

// parseMediaContentType is a tiny test helper that pulls the boundary out of a
// multipart content type without importing mime at the package level.
func parseMediaContentType(ct string) (string, map[string]string, error) {
	idx := strings.Index(ct, "boundary=")
	if idx < 0 {
		return "", nil, fmt.Errorf("no boundary in %q", ct)
	}
	return "multipart/form-data", map[string]string{"boundary": ct[idx+len("boundary="):]}, nil
}

func TestParseMediaInit(t *testing.T) {
	id, err := parseMediaInit([]byte(`{"media_id":123,"media_id_string":"123","expires_after_secs":86400}`))
	if err != nil {
		t.Fatal(err)
	}
	if id != "123" {
		t.Fatalf("expected media id 123, got %q", id)
	}

	if _, err := parseMediaInit([]byte(`{"errors":[{"code":324,"message":"bad media"}]}`)); err == nil {
		t.Fatal("expected error for INIT error response")
	}

	if _, err := parseMediaInit([]byte(`{}`)); err == nil {
		t.Fatal("expected error for empty media id")
	}
}

func TestParseProcessingInfo(t *testing.T) {
	info, err := parseProcessingInfo([]byte(`{"media_id_string":"1","processing_info":{"state":"pending","check_after_secs":5}}`))
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.State != "pending" || info.CheckAfter != 5 {
		t.Fatalf("expected pending/5, got %+v", info)
	}

	// Image finalize: no processing_info present.
	info, err = parseProcessingInfo([]byte(`{"media_id_string":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if info != nil {
		t.Fatalf("expected nil processing_info, got %+v", info)
	}

	// Empty body (e.g. 204 APPEND).
	info, err = parseProcessingInfo(nil)
	if err != nil {
		t.Fatal(err)
	}
	if info != nil {
		t.Fatalf("expected nil for empty body, got %+v", info)
	}

	// Error response.
	if _, err := parseProcessingInfo([]byte(`{"errors":[{"code":1,"message":"boom"}]}`)); err == nil {
		t.Fatal("expected error for error response")
	}
}

func TestProcessingDone(t *testing.T) {
	tests := []struct {
		name     string
		info     *processingInfo
		wantDone bool
		wantErr  bool
	}{
		{"nil (image)", nil, true, false},
		{"succeeded", &processingInfo{State: "succeeded"}, true, false},
		{"empty state", &processingInfo{State: ""}, true, false},
		{"pending", &processingInfo{State: "pending"}, false, false},
		{"in_progress", &processingInfo{State: "in_progress"}, false, false},
		{"failed", &processingInfo{State: "failed", Progress: 30}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, err := processingDone(tt.info)
			if done != tt.wantDone {
				t.Fatalf("done = %v, want %v", done, tt.wantDone)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCreateTweetVariables(t *testing.T) {
	// No media -> empty media_entities (matches legacy CreateTweet shape).
	v := createTweetVariables("hello", nil)
	media := v["media"].(map[string]any)
	if entities := media["media_entities"].([]any); len(entities) != 0 {
		t.Fatalf("expected 0 media entities, got %d", len(entities))
	}
	if v["tweet_text"] != "hello" {
		t.Fatalf("expected tweet_text 'hello', got %v", v["tweet_text"])
	}

	// With media -> one entity per id, each with media_id + tagged_users.
	v = createTweetVariables("hi", []string{"111", "222"})
	entities := v["media"].(map[string]any)["media_entities"].([]any)
	if len(entities) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(entities))
	}
	first := entities[0].(map[string]any)
	if first["media_id"] != "111" {
		t.Fatalf("expected media_id 111, got %v", first["media_id"])
	}
	if _, ok := first["tagged_users"]; !ok {
		t.Fatal("expected tagged_users key on media entity")
	}
}

func TestReplyTweetVariables(t *testing.T) {
	// Reply slot points at the parent tweet, with an empty exclude list.
	v := replyTweetVariables("re: hello", "1500000000000000003", nil)
	reply, ok := v["reply"].(map[string]any)
	if !ok {
		t.Fatal("expected reply slot present on reply variables")
	}
	if reply["in_reply_to_tweet_id"] != "1500000000000000003" {
		t.Fatalf("expected in_reply_to_tweet_id set, got %v", reply["in_reply_to_tweet_id"])
	}
	if _, ok := reply["exclude_reply_user_ids"].([]any); !ok {
		t.Fatal("expected exclude_reply_user_ids slice present")
	}

	// Media slot stays present and pluggable (not removed) — empty by default so a
	// future ReplyWithMedia can populate media_entities (plan acceptance #5).
	media, ok := v["media"].(map[string]any)
	if !ok {
		t.Fatal("expected media slot present (pluggable) on reply variables")
	}
	if entities := media["media_entities"].([]any); len(entities) != 0 {
		t.Fatalf("expected 0 media entities by default, got %d", len(entities))
	}
	if v["tweet_text"] != "re: hello" {
		t.Fatalf("expected tweet_text preserved, got %v", v["tweet_text"])
	}

	// Media IDs flow into the same media slot (proves the slot is pluggable).
	v = replyTweetVariables("re: pic", "999", []string{"7777"})
	entities := v["media"].(map[string]any)["media_entities"].([]any)
	if len(entities) != 1 {
		t.Fatalf("expected 1 media entity, got %d", len(entities))
	}
	if entities[0].(map[string]any)["media_id"] != "7777" {
		t.Fatalf("expected media_id 7777 attached to reply, got %v", entities[0])
	}
}

func TestRunMediaUpload_ImageNoPolling(t *testing.T) {
	var statusCalls int
	do := func(_ context.Context, _, urlStr string, _ []byte, _ string) ([]byte, error) {
		switch commandOf(urlStr) {
		case "INIT":
			return []byte(`{"media_id_string":"555"}`), nil
		case "APPEND":
			return nil, nil
		case "FINALIZE":
			return []byte(`{"media_id_string":"555"}`), nil // no processing_info
		case "STATUS":
			statusCalls++
			return []byte(`{"processing_info":{"state":"succeeded"}}`), nil
		}
		return nil, fmt.Errorf("unexpected command")
	}

	id, err := runMediaUpload(context.Background(), do, func(context.Context, time.Duration) error { return nil }, "https://upload.example/x", []byte("imgbytes"), "image/jpeg", 4)
	if err != nil {
		t.Fatal(err)
	}
	if id != "555" {
		t.Fatalf("expected id 555, got %q", id)
	}
	if statusCalls != 0 {
		t.Fatalf("image should not poll STATUS, got %d calls", statusCalls)
	}
}

func TestRunMediaUpload_VideoPolling(t *testing.T) {
	var statusCalls, sleeps int
	do := func(_ context.Context, _, urlStr string, _ []byte, _ string) ([]byte, error) {
		switch commandOf(urlStr) {
		case "INIT":
			return []byte(`{"media_id_string":"999"}`), nil
		case "APPEND":
			return nil, nil
		case "FINALIZE":
			return []byte(`{"processing_info":{"state":"pending","check_after_secs":0}}`), nil
		case "STATUS":
			statusCalls++
			if statusCalls >= 2 {
				return []byte(`{"processing_info":{"state":"succeeded","progress_percent":100}}`), nil
			}
			return []byte(`{"processing_info":{"state":"in_progress","check_after_secs":0}}`), nil
		}
		return nil, fmt.Errorf("unexpected command")
	}
	sleep := func(context.Context, time.Duration) error { sleeps++; return nil }

	id, err := runMediaUpload(context.Background(), do, sleep, "https://upload.example/x", bytes.Repeat([]byte("v"), 10), "video/mp4", 4)
	if err != nil {
		t.Fatal(err)
	}
	if id != "999" {
		t.Fatalf("expected id 999, got %q", id)
	}
	if statusCalls < 2 {
		t.Fatalf("expected >=2 STATUS polls, got %d", statusCalls)
	}
	if sleeps < 2 {
		t.Fatalf("expected >=2 sleeps between polls, got %d", sleeps)
	}
}

func TestRunMediaUpload_Failed(t *testing.T) {
	do := func(_ context.Context, _, urlStr string, _ []byte, _ string) ([]byte, error) {
		switch commandOf(urlStr) {
		case "INIT":
			return []byte(`{"media_id_string":"1"}`), nil
		case "APPEND":
			return nil, nil
		case "FINALIZE":
			return []byte(`{"processing_info":{"state":"failed","error":{"message":"InvalidMedia"}}}`), nil
		}
		return nil, fmt.Errorf("unexpected command")
	}
	if _, err := runMediaUpload(context.Background(), do, func(context.Context, time.Duration) error { return nil }, "https://upload.example/x", []byte("x"), "video/mp4", 4); err == nil {
		t.Fatal("expected error on failed processing")
	}
}

// TestRunMediaUpload_PollContextCanceled verifies that a context cancelled
// during the STATUS poll makes runMediaUpload return ctx.Err() promptly,
// without exhausting the maxMediaPollAttempts (60) loop.
func TestRunMediaUpload_PollContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var statusCalls int
	do := func(_ context.Context, _, urlStr string, _ []byte, _ string) ([]byte, error) {
		switch commandOf(urlStr) {
		case "INIT":
			return []byte(`{"media_id_string":"42"}`), nil
		case "APPEND":
			return nil, nil
		case "FINALIZE":
			return []byte(`{"processing_info":{"state":"in_progress","check_after_secs":0}}`), nil
		case "STATUS":
			statusCalls++
			return []byte(`{"processing_info":{"state":"in_progress","check_after_secs":0}}`), nil
		}
		return nil, fmt.Errorf("unexpected command")
	}
	// Cancel on the first poll wait, then honor the context like the
	// production sleeper does.
	sleep := func(ctx context.Context, d time.Duration) error {
		cancel()
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	_, err := runMediaUpload(ctx, do, sleep, "https://upload.example/x", []byte("vid"), "video/mp4", 4)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if statusCalls != 0 {
		t.Fatalf("expected no STATUS polls after cancellation, got %d", statusCalls)
	}
}

func TestRunMediaUpload_HTTPEndToEnd(t *testing.T) {
	var mu sync.Mutex
	segments := map[int][]byte{}
	var statusCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("command") {
		case "INIT":
			_, _ = w.Write([]byte(`{"media_id_string":"777","expires_after_secs":86400}`))
		case "APPEND":
			idx, _ := strconv.Atoi(r.URL.Query().Get("segment_index"))
			f, _, err := r.FormFile("media")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			b, _ := io.ReadAll(f)
			mu.Lock()
			segments[idx] = b
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "FINALIZE":
			_, _ = w.Write([]byte(`{"media_id_string":"777","processing_info":{"state":"pending","check_after_secs":0}}`))
		case "STATUS":
			mu.Lock()
			statusCalls++
			n := statusCalls
			mu.Unlock()
			if n >= 2 {
				_, _ = w.Write([]byte(`{"processing_info":{"state":"succeeded","progress_percent":100}}`))
			} else {
				_, _ = w.Write([]byte(`{"processing_info":{"state":"in_progress","check_after_secs":0}}`))
			}
		default:
			http.Error(w, "bad command", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	do := func(ctx context.Context, method, urlStr string, body []byte, contentType string) ([]byte, error) {
		var rdr io.Reader
		if len(body) > 0 {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, urlStr, rdr)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, b)
		}
		return b, nil
	}

	data := []byte("abcdefghijklmnopqrstuvwxyz0123") // 30 bytes
	id, err := runMediaUpload(context.Background(), do, func(context.Context, time.Duration) error { return nil }, srv.URL, data, "video/mp4", 7)
	if err != nil {
		t.Fatal(err)
	}
	if id != "777" {
		t.Fatalf("expected id 777, got %q", id)
	}
	if statusCalls < 2 {
		t.Fatalf("expected >=2 STATUS polls, got %d", statusCalls)
	}

	var got []byte
	mu.Lock()
	for i := 0; i < len(segments); i++ {
		got = append(got, segments[i]...)
	}
	mu.Unlock()
	if !bytes.Equal(got, data) {
		t.Fatalf("reassembled upload = %q, want %q", got, data)
	}
}

func TestMediaTypeForFile_SniffFallback(t *testing.T) {
	// Extensionless path -> falls back to content sniffing.
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00,")
	if mt := mediaTypeForFile("clip", gif); mt != "image/gif" {
		t.Fatalf("expected image/gif from sniff, got %q", mt)
	}
}

// commandOf extracts the ?command= value from a media upload URL (test helper).
func commandOf(urlStr string) string {
	u, err := url.Parse(urlStr)
	if err != nil {
		return ""
	}
	return u.Query().Get("command")
}
