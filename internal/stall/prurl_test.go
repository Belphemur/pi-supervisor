package stall

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPRURLFromFindsLinkInWholeTranscript is the ADR-0006 regression.
//
// The streaming Detector only sees bytes appended after New(), and it is only
// constructed once a transcript path is pinned. On a fresh LAUNCH — the round
// that opens the PR and the round that finishes it are often the same one —
// nothing scraped the link, so the review auto-trigger emitted
// review_skipped ("no GitHub PR was linked") for a transcript that plainly
// contained the URL.
//
// Uses the real transcript shape from the live failure.
func TestPRURLFromFindsLinkInWholeTranscript(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"do the work"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"tool_use","name":"bash"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"opened https://github.com/Belphemur/pi-supervisor/pull/4"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"ALL_DONE"}]}}`,
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := PRURLFrom(p)
	want := "https://github.com/Belphemur/pi-supervisor/pull/4"
	if got != want {
		t.Fatalf("PRURLFrom = %q, want %q", got, want)
	}
}

// A transcript with no PR link stays "" — "not linked", never "no PR exists"
// (ADR-0006). It must not be an error and must not invent a URL.
func TestPRURLFromNoLinkIsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(p, []byte(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"all done"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := PRURLFrom(p); got != "" {
		t.Fatalf("PRURLFrom = %q, want empty", got)
	}
	// A missing file is also "" rather than a panic.
	if got := PRURLFrom(filepath.Join(dir, "nope.jsonl")); got != "" {
		t.Fatalf("missing file = %q, want empty", got)
	}
}

// A truncated URL must not match: digits are required, so ".../pull/" alone is
// never adopted as a PR.
func TestPRURLFromRejectsTruncated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(p, []byte(`{"text":"see https://github.com/o/r/pull/"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := PRURLFrom(p); got != "" {
		t.Fatalf("truncated URL matched: %q", got)
	}
}
