package provision

import (
	"encoding/json"
	"testing"

	"github.com/3th1nk/mammoth/internal/store"
)

// The rescue context/report share the install context's JSON keys for the
// media facts (token/media_uri) — the shared cleanup paths parse them
// without knowing the flow — while the report lands under the "rescue" key.
func TestParseRescueContextAndRecord(t *testing.T) {
	ctxJSON := `{"token":"tok1","media_uri":"http://e/boot/boot-tok1.iso","install":{"booted_at":"2026-01-01T00:00:00Z"}}`
	task := &store.Task{Context: json.RawMessage(ctxJSON)}

	rctx := parseRescueContext(task)
	if rctx.Token != "tok1" || rctx.MediaURI == "" {
		t.Fatalf("context parse: %+v", rctx)
	}
	if rec := parseRescueRecord(task); rec != nil {
		t.Fatalf("no rescue record yet, got %+v", rec)
	}

	task.Context = json.RawMessage(`{"token":"tok1","rescue":{"ready_at":"2026-01-01T00:01:00Z","status":"ok","detail":"up"}}`)
	rec := parseRescueRecord(task)
	if rec == nil || rec.Status != "ok" || rec.Detail != "up" {
		t.Fatalf("record parse: %+v", rec)
	}
	if rec.ReadyAt == nil {
		t.Fatal("ready_at must parse")
	}

	// corrupt context: cleanup paths must never fail on it
	task.Context = json.RawMessage(`{broken`)
	if rctx := parseRescueContext(task); rctx.Token != "" {
		t.Fatalf("corrupt context must parse empty, got %+v", rctx)
	}
	if rec := parseRescueRecord(task); rec != nil {
		t.Fatalf("corrupt context must yield no record, got %+v", rec)
	}
}
