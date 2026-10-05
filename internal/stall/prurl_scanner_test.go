package stall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPRURLScannerFindsLinkAcrossIncrementalReads is the ADR-0006 correctness
// property: a link written EARLY must still be found after the transcript grows
// far past the tail window. The one-shot tail-only version dropped it, reporting
// "not linked" for a transcript that plainly contained the URL.
//
// The scanner is read repeatedly as the file grows, exactly as the completion
// gate does across rounds.
func TestPRURLScannerFindsLinkAcrossIncrementalReads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	want := "https://github.com/Belphemur/pi-supervisor/pull/4"

	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"opened ` + want + `"}]}}` + "\n")
	sc := NewPRURLScanner(p)
	if got := sc.PRURL(); got != want {
		t.Fatalf("first read = %q, want %q", got, want)
	}

	// Grow well past prURLTailBytes with content containing no PR link.
	filler := strings.Repeat("x", 64<<10)
	for range 24 { // 24 * 64KiB == 1.5MiB
		if _, err := f.WriteString(`{"type":"message","message":{"role":"toolResult","content":[{"type":"text","text":"` + filler + `"}]}}` + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if got := sc.PRURL(); got != want {
		t.Fatalf("after 1.5MiB of growth the scanner forgot the link: %q", got)
	}
	_ = f.Close()

	// A link appended at the very end wins, since it is the latest. Note the
	// CATCH-UP LOOP: one call consumes at most prURLChunkBytes, so scanning a
	// region larger than that needs several calls. That is the deliberate
	// trade — bounding a read must not mean bounding what gets scanned, which
	// is the bug this replaced (jumping to the tail skipped the middle forever).
	f2, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	newer := "https://github.com/Belphemur/pi-supervisor/pull/9"
	_, _ = f2.WriteString(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"also ` + newer + `"}]}}` + "\n")
	_ = f2.Close()
	var got string
	for range 64 { // 64 * 256KiB == 16MiB, far more than the 1.5MiB backlog
		got = sc.PRURL()
		if got == newer {
			break
		}
	}
	if got != newer {
		t.Fatalf("scanner = %q after catching up, want the LAST link %q", got, newer)
	}
}

// TestPRURLScannerReadsOnlyNewBytes is the cost property: the whole point of the
// offset. A second call with no growth must read nothing at all, which is what
// stops a build-only job from re-reading a growing transcript every round.
func TestPRURLScannerReadsOnlyNewBytes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{}\n")
	sc := NewPRURLScanner(p)
	sc.PRURL()

	// Grow well past one chunk with filler that has no link, then read until the
	// backlog is drained. Draining first is required: while unread bytes remain,
	// a call legitimately keeps reading (that is the fix for the skipped-middle
	// bug), so "no growth" only means a no-op once everything is consumed.
	filler := strings.Repeat("x", 64<<10)
	for range 24 {
		_, _ = f.WriteString(filler + "\n")
	}
	for range 64 {
		sc.mu.Lock()
		off := sc.offset
		sc.mu.Unlock()
		if fi, err := os.Stat(p); err == nil && off >= fi.Size() {
			break
		}
		sc.PRURL()
	}

	// Record the offset, then assert a no-growth call is a no-op.
	sc.mu.Lock()
	offBefore := sc.offset
	sizeBefore := sc.lastSize
	sc.mu.Unlock()

	sc.PRURL() // no growth since the last call

	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.lastSize != sizeBefore {
		t.Fatalf("lastSize moved without growth: %d -> %d", sizeBefore, sc.lastSize)
	}
	if sc.offset != offBefore {
		t.Fatalf("offset moved without growth: %d -> %d", offBefore, sc.offset)
	}
}

// A truncated/rotated transcript must not be read from a stale offset, and must
// still find a link in the NEW content.
func TestPRURLScannerResetsOnTruncate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Repeat("y", 40<<10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := NewPRURLScanner(p)
	sc.PRURL() // advances the offset into the filler

	// pi rotates the file: smaller, with a link.
	want := "https://github.com/Belphemur/pi-supervisor/pull/7"
	if err := os.WriteFile(p, []byte(`{"text":"`+want+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sc.PRURL(); got != want {
		t.Fatalf("after truncate = %q, want %q (stale offset was not reset)", got, want)
	}
}

// Path reports the transcript, so a caller can drop a scanner whose session
// changed (restart --fresh).
func TestPRURLScannerPath(t *testing.T) {
	sc := NewPRURLScanner("/tmp/a.jsonl")
	if sc.Path() != "/tmp/a.jsonl" {
		t.Fatalf("Path = %q", sc.Path())
	}
}
