package stall

import (
	"path/filepath"
	"testing"
	"time"
)

// ADR-0023: the supervisor's recordPR gate validates a scraped URL against the
// worktree's git remotes. A REJECTED url is fed back via SkipPRURL so the
// scraper keeps looking instead of latching garbage forever — the first URL
// wins only among ACCEPTED urls.

// Detector: a non-matching link is skipped, and a later matching link latches.
func TestDetectorSkipsRejectedURLAndKeepsLooking(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writePRLines(t, p, `{"content":"seed"}`) // file exists; detector tails from here
	d := New(p, time.Hour)
	// A non-matching link is scraped, then rejected by the gate.
	writePRLines(t, p,
		`{"content":"see https://github.com/nuxt/nuxt/pull/36542 for context"}`)
	d.Poll()
	foreign := "https://github.com/nuxt/nuxt/pull/36542"
	if got := d.PRURL(); got != foreign {
		t.Fatalf("PRURL = %q, want %q (the scrape itself must still find it)", got, foreign)
	}

	d.SkipPRURL(foreign)
	if got := d.PRURL(); got != "" {
		t.Fatalf("PRURL after skip = %q, want empty", got)
	}

	// A later, matching link latches — the scraper kept looking.
	writePRLines(t, p,
		`{"content":"opened https://github.com/owner/project/pull/7"}`)
	d.Poll()
	if got := d.PRURL(); got != "https://github.com/owner/project/pull/7" {
		t.Fatalf("PRURL = %q, want the matching link", got)
	}
}

// Detector: skipping an url that was never latched is a no-op, and a skipped
// url is never re-latched even when it appears again in later bytes.
func TestDetectorSkipIsSticky(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writePRLines(t, p, `{"content":"nothing yet"}`)
	d := New(p, time.Hour)
	d.Poll()
	d.SkipPRURL("https://github.com/nuxt/nuxt/pull/1") // never seen: no-op

	writePRLines(t, p, `{"content":"again https://github.com/nuxt/nuxt/pull/1"}`)
	d.Poll()
	if got := d.PRURL(); got != "" {
		t.Fatalf("skipped url re-latched: %q", got)
	}
}

// PRURLScanner (the last-link scanner): a rejected last link is forgotten and
// a later matching link replaces it.
func TestScannerSkipsRejectedURLAndKeepsLooking(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writePRLines(t, p,
		`{"content":"see https://github.com/nuxt/nuxt/pull/36542"}`)
	s := NewPRURLScanner(p)
	foreign := "https://github.com/nuxt/nuxt/pull/36542"
	if got := s.PRURL(); got != foreign {
		t.Fatalf("PRURL = %q, want %q", got, foreign)
	}

	s.SkipPRURL(foreign)
	if got := s.PRURL(); got != "" {
		t.Fatalf("PRURL after skip = %q, want empty", got)
	}

	writePRLines(t, p,
		`{"content":"opened https://github.com/owner/project/pull/7"}`)
	if got := s.PRURL(); got != "https://github.com/owner/project/pull/7" {
		t.Fatalf("PRURL = %q, want the matching link", got)
	}
}

// Scanner: within ONE read, the last NON-skipped link wins — a garbage link
// after the real one does not hide it.
func TestScannerSkippedURLDoesNotHideLaterLink(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writePRLines(t, p,
		`{"content":"opened https://github.com/owner/project/pull/7 then cited https://github.com/nuxt/nuxt/pull/36542"}`)
	s := NewPRURLScanner(p)
	s.SkipPRURL("https://github.com/nuxt/nuxt/pull/36542")
	if got := s.PRURL(); got != "https://github.com/owner/project/pull/7" {
		t.Fatalf("PRURL = %q, want the non-skipped link", got)
	}
}

// ADR-0023 scrape-side: when the newest latched URL is rejected, the Scanner
// rewinds its read window and re-picks an earlier, non-skipped link from the
// SAME read — a surviving link is not committed past by an incremental offset.
// Without the rewind, SkipPRURL just clears the latch and the next PRURL()
// short-circuits on the cached "" (no new bytes), so the earlier link is lost.
func TestScannerRejectedNewestRewindsToEarlierLink(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	// #7 (matching) is written BEFORE nuxt/36542 (foreign); nuxt is newest, so
	// the backward scan latches it first.
	writePRLines(t, p, `{"content":"opened https://github.com/owner/project/pull/7 while reading https://github.com/nuxt/nuxt/pull/36542"}`)
	s := NewPRURLScanner(p)

	newest := "https://github.com/nuxt/nuxt/pull/36542"
	if got := s.PRURL(); got != newest {
		t.Fatalf("PRURL = %q, want the newest link %q", got, newest)
	}

	// Reject the newest; the rewind must surface the surviving earlier link.
	s.SkipPRURL(newest)
	if got := s.PRURL(); got != "https://github.com/owner/project/pull/7" {
		t.Fatalf("PRURL after skip = %q, want the surviving earlier link", got)
	}
}
