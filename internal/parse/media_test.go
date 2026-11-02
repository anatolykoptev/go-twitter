package parse

import "testing"

func TestParseMediaInit(t *testing.T) {
	id, err := MediaInit([]byte(`{"media_id":123,"media_id_string":"123","expires_after_secs":86400}`))
	if err != nil {
		t.Fatal(err)
	}
	if id != "123" {
		t.Fatalf("expected media id 123, got %q", id)
	}

	if _, err := MediaInit([]byte(`{"errors":[{"code":324,"message":"bad media"}]}`)); err == nil {
		t.Fatal("expected error for INIT error response")
	}

	if _, err := MediaInit([]byte(`{}`)); err == nil {
		t.Fatal("expected error for empty media id")
	}
}

func TestParseProcessingInfo(t *testing.T) {
	info, err := MediaProcessing([]byte(`{"media_id_string":"1","processing_info":{"state":"pending","check_after_secs":5}}`))
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.State != "pending" || info.CheckAfter != 5 {
		t.Fatalf("expected pending/5, got %+v", info)
	}

	// Image finalize: no processing_info present.
	info, err = MediaProcessing([]byte(`{"media_id_string":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if info != nil {
		t.Fatalf("expected nil processing_info, got %+v", info)
	}

	// Empty body (e.g. 204 APPEND).
	info, err = MediaProcessing(nil)
	if err != nil {
		t.Fatal(err)
	}
	if info != nil {
		t.Fatalf("expected nil for empty body, got %+v", info)
	}

	// Error response.
	if _, err := MediaProcessing([]byte(`{"errors":[{"code":1,"message":"boom"}]}`)); err == nil {
		t.Fatal("expected error for error response")
	}
}
