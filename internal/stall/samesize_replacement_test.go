package stall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The pi transcript is sometimes REPLACED rather than appended: a rotation or
// restart --fresh swaps in a new file (rename, the way pi and log rotation
// actually do it) that can have EXACTLY the same length as the old one. The
// "nothing appended" short-circuit compares size alone, so this scenario served
// the old file's link forever (kody PR #5; the inode-based fix it replaced was
// dead code — on Unix Ino is a FIELD of *syscall.Stat_t, not an Ino() method,
// so the interface assertion matched nothing).
//
// The replacement is performed by rename, deliberately: os.WriteFile over the
// same path truncates the EXISTING file and keeps its inode, which no identity
// check can distinguish — measured (same inode both writes) and out of scope,
// because pi never rewrites a session JSONL in place, it rotates by rename.
func TestScannerSameSizeReplacement(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")

	old := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"opened https://github.com/Belphemur/pi-supervisor/pull/4"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := NewPRURLScanner(p)
	_ = sc.PRURL() // establishes offset, lastSize and lastIno on the OLD file

	// Build a same-length file whose link is the NEW one. The old link must not
	// survive the swap.
	want := "https://github.com/Belphemur/pi-supervisor/pull/77"
	newBody := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"updated ` + want + `"}]}}` + "\n"
	switch {
	case len(newBody) < len(old):
		pad := `{"pad":"` + strings.Repeat("x", len(old)-len(newBody)-13) + `"}`
		newBody += pad + "\n"
		newBody = newBody[:len(old)]
	case len(newBody) > len(old):
		newBody = newBody[:len(old)-1] + "\n"
	}
	if len(newBody) != len(old) {
		t.Fatalf("setup: lengths differ old=%d new=%d", len(old), len(newBody))
	}
	if !strings.Contains(newBody, want) {
		t.Fatalf("setup: new body lost the new link: %.120q", newBody)
	}
	// Swap the file in via rename — the rotation pi actually performs. A same-path
	// truncate would keep the inode and be undetectable by design (see above).
	tmp := p + ".new"
	if err := os.WriteFile(tmp, []byte(newBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}

	var got string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got = sc.PRURL()
		if got == want {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got != want {
		t.Fatalf("same-length replacement: stale link %q was served, want %q (the old file's link survived the rewrite)", got, want)
	}
}

// A URL that STRADDLES the chunk boundary — inside one very long JSON line —
// must still be found: the overlap re-read covers the byte range before the
// raw-end offset, and no partial-line trimming exists anymore. Pins the kody
// finding that the previous trim+midline-commit variant dropped such links, and
// that the raw scan + unconditional commit survives it.
func TestScannerStraddlesChunkBoundary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{}\n")
	sc := NewPRURLScanner(p)
	sc.PRURL() // offset after "{}\n"

	// One enormous single line whose tail holds the link; the chunk boundary
	// falls inside the JSON filler by construction (300KiB+ > 256KiB chunk).
	want := "https://github.com/Belphemur/pi-supervisor/pull/12"
	_, _ = f.WriteString(`{"filler":"` + strings.Repeat("x", 300<<10) + `","link":"` + want + `"}` + "\n")
	_ = f.Close()

	var got string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got = sc.PRURL()
		if got == want {
			break
		}
	}
	if got != want {
		t.Fatalf("straddling link missed: got %q, want %q", got, want)
	}
}
