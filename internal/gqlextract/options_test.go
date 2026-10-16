package gqlextract

import (
	"io"
	"log/slog"
	"maps"
	"testing"
)

// TestExtractor_QueryIDShapeOnly proves the runtime option drops the params
// (Relay persisted-query) shape while keeping the {queryId, operationName}
// shape, and that the default keeps both (the codegen behaviour).
func TestExtractor_QueryIDShapeOnly(t *testing.T) {
	body := []byte(`var ops=[{queryId:"AAA111",operationName:"OpOne"}];` +
		`e.exports={n:{params:{id:"RELAYHASH",name:"OpTwo",operationKind:"mutation"}}};`)

	both := quietExtractor()
	both.Consume(rwAPI, body)
	if want := (map[string]string{"OpOne": "AAA111", "OpTwo": "RELAYHASH"}); !maps.Equal(both.Result(), want) {
		t.Fatalf("default result = %v, want %v", both.Result(), want)
	}

	only := NewExtractor(slog.New(slog.NewTextHandler(io.Discard, nil)), QueryIDShapeOnly())
	only.Consume(rwAPI, body)
	if want := (map[string]string{"OpOne": "AAA111"}); !maps.Equal(only.Result(), want) {
		t.Fatalf("QueryIDShapeOnly result = %v, want %v (the Relay hash must not pass as a queryId)", only.Result(), want)
	}
}

// TestExtractor_InvalidTracked proves a dropped malformed id is remembered, so
// a caller can refuse a tampered bundle instead of trusting its other ops.
func TestExtractor_InvalidTracked(t *testing.T) {
	ext := quietExtractor()
	ext.Consume(rwAPI, []byte(`queryId:"BAD/ID",operationName:"PoisonOp";queryId:"OKID",operationName:"CleanOp"`))
	if !ext.Invalid("PoisonOp") {
		t.Error("PoisonOp: malformed id not recorded")
	}
	if ext.Invalid("CleanOp") {
		t.Error("CleanOp wrongly flagged invalid")
	}
}
