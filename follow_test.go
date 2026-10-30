package twitter

import (
	"strings"
	"testing"
)

func TestParseFollowResponse(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string // "" = success
	}{
		{"ok", 200, `{"id_str":"123","screen_name":"x"}`, ""},
		{"x error entry", 403, `{"errors":[{"code":160,"message":"already requested"}]}`, "X error 160: already requested"},
		{"error entry wins over 200", 200, `{"errors":[{"code":162,"message":"blocked"}]}`, "X error 162"},
		{"non-200 no body", 404, ``, "HTTP 404"},
		{"200 without user", 200, `{}`, "HTTP 200"},
	}
	for _, c := range cases {
		err := parseFollowResponse(c.status, []byte(c.body))
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.wantErr)
		}
	}
}
