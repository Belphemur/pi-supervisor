package stall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScannerFindsLinkInLongTranscript is the ADR-0006 case, at a size that
// breaks each of the two implementations that shipped first:
//
//   - the tail-only scan missed it (512 KiB window, link in line 1);
//   - the whole-file scan found it but re-read every byte on every round.
//
// The scanner must find it AND read only forward from its offset, so the cost is
// proportional to what was appended rather than to the transcript's age.
func TestScannerFindsLinkInLongTranscript(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	var b strings.Builder
	b.WriteString(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"opened https://github.com/Belphemur/pi-supervisor/pull/4"}]}}` + "\n")
	filler := strings.Repeat("x", 64<<10)
	for range 24 { // 1.5 MiB — three times the old window
		b.WriteString(`{"type":"message","message":{"role":"toolResult","content":[{"type":"text","text":"` + filler + `"}]}}` + "\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewPRURLScanner(p)
	want := "https://github.com/Belphemur/pi-supervisor/pull/4"
	if got := s.PRURL(); got != want {
		t.Fatalf("PRURL = %q, want %q", got, want)
	}
	// A second call must not re-read the 1.5 MiB already scanned.
	if got := s.PRURL(); got != want {
		t.Fatalf("PRURL on rescan = %q, want %q", got, want)
	}
}
