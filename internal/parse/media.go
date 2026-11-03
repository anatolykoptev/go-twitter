package parse

import (
	"encoding/json"
	"fmt"
)

// ProcessingInfo mirrors Twitter's async media processing state returned by
// FINALIZE and STATUS. Images return no processing_info; video/gif do.
type ProcessingInfo struct {
	State      string `json:"state"`
	CheckAfter int    `json:"check_after_secs"`
	Progress   int    `json:"progress_percent"`
	Error      *struct {
		Code    int    `json:"code"`
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"error"`
}

// MediaInit extracts media_id_string from an INIT response.
func MediaInit(body []byte) (string, error) {
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
		return "", fmt.Errorf("media INIT returned empty media_id: %s", TruncateBytes(body, 200))
	}
	return raw.MediaIDString, nil
}

// MediaProcessing extracts processing_info from a FINALIZE/STATUS response.
// Returns (nil, nil) when no processing_info is present (e.g. images, or 204).
func MediaProcessing(body []byte) (*ProcessingInfo, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var raw struct {
		ProcessingInfo *ProcessingInfo `json:"processing_info"`
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
