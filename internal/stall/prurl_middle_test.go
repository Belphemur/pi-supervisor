package stall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScannerDoesNotSkipTheMiddle is the regression for a bug the previous
// implementation had: when more than one chunk arrived between two calls, it
// jumped the read to `size - prURLTailBytes` and permanently skipped everything
// in between. A link written into that gap was never scanned, so the gate
// reported "not linked" for a job that opened its PR early and then emitted a
// lot in one round — the ADR-0006 failure all over again.
//
// The link here is written AFTER the first scan, then buried behind more than a
// chunk of output, so any tail-jumping implementation misses it.
func TestScannerDoesNotSkipTheMiddle(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{}\n")
	sc := NewPRURLScanner(p)
	sc.PRURL() // establish an offset

	// The link lands next.
	want := "https://github.com/Belphemur/pi-supervisor/pull/7"
	_, _ = f.WriteString(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"opened ` + want + `"}]}}` + "\n")

	// Then a single burst far larger than one chunk, burying it.
	filler := strings.Repeat("y", 64<<10)
	for range 40 { // 2.5 MiB in one write burst
		_, _ = f.WriteString(`{"type":"message","message":{"role":"toolResult","content":[{"type":"text","text":"` + filler + `"}]}}` + "\n")
	}

	// Reading until the backlog drains must surface the buried link.
	var got string
	for range 64 {
		got = sc.PRURL()
		if got == want {
			break
		}
	}
	if got != want {
		t.Fatalf("scanner never found the buried link: got %q, want %q — the middle was skipped", got, want)
	}
}
