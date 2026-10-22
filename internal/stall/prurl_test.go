package stall

import (
	"os"
	"path/filepath"
	"testing"
)

// These cover the ADR-0006 properties on the INCREMENTAL scanner, which is what
// production uses. The one-shot PRURLFrom they used to exercise is gone: the
// gate keeps a scanner per job, so the one-shot form had no production caller.

// writePRLines appends JSONL lines to p, mirroring how a live session
// transcript grows under pi (append-only tail). Truncating would reset the
// byte offsets these scanners tail on and hide already-written content from a
// Detector created at EOF (see TestDetectorSkipsRejectedURLAndKeepsLooking).
func writePRLines(t *testing.T, p string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// The link sits in assistant text, which is where an agent states it.
func TestScannerFindsLinkInAssistantText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writePRLines(t, p,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"do the work"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"tool_use","name":"bash"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"opened https://github.com/Belphemur/pi-supervisor/pull/4"}]}}`,
	)
	s := NewPRURLScanner(p)
	want := "https://github.com/Belphemur/pi-supervisor/pull/4"
	if got := s.PRURL(); got != want {
		t.Fatalf("PRURL = %q, want %q", got, want)
	}
}

// No link stays "" — "not linked", never "no PR exists" (ADR-0006). A missing
// file is also "" rather than a panic.
func TestScannerNoLinkIsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writePRLines(t, p, `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"all done"}]}}`)
	if got := NewPRURLScanner(p).PRURL(); got != "" {
		t.Fatalf("PRURL = %q, want empty", got)
	}
	if got := NewPRURLScanner(filepath.Join(dir, "nope.jsonl")).PRURL(); got != "" {
		t.Fatalf("missing file = %q, want empty", got)
	}
}

// Digits are required, so a truncated ".../pull/" is never adopted as a PR.
func TestScannerRejectsTruncated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writePRLines(t, p, `{"text":"see https://github.com/o/r/pull/"}`)
	if got := NewPRURLScanner(p).PRURL(); got != "" {
		t.Fatalf("truncated URL matched: %q", got)
	}
}
