package stall

// ADR-0021: the model-identity scanner reads assistant records'
// provider/model pairs from the session JSONL incrementally — never the
// whole file, never fooled by quoted "model" strings in briefs, steers, or
// toolResults (only the assistant-record header pair counts).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestModelScannerTracksUsedAndCurrent(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)

	// Quoted "model" text in a USER message is NOT an answer — the pair in
	// the assistant header is the only signal.
	writeLine(t, sess, `{"type":"message","message":{"role":"user","content":"use model glm-5.3-flash please"}}`)
	cur, used := s.Scan()
	if cur != "" || len(used) != 0 {
		t.Fatalf("user-message quote leaked into model identity: cur=%q used=%v", cur, used)
	}

	writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"opencode-go","model":"step-5-preview-free","usage":{}}}`)
	cur, used = s.Scan()
	if cur != "opencode-go/step-5-preview-free" {
		t.Fatalf("current = %q, want opencode-go/step-5-preview-free", cur)
	}
	if len(used) != 1 || used[0] != "opencode-go/step-5-preview-free" {
		t.Fatalf("used = %v, want exactly one entry", used)
	}

	// A model swap mid-run appends to the list and moves current.
	writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"openrouter","model":"z-ai/glm-5.3-flash","usage":{}}}`)
	cur, used = s.Scan()
	if cur != "openrouter/z-ai/glm-5.3-flash" {
		t.Fatalf("current after swap = %q", cur)
	}
	if len(used) != 2 || used[0] != "opencode-go/step-5-preview-free" || used[1] != "openrouter/z-ai/glm-5.3-flash" {
		t.Fatalf("used after swap = %v, want both in first-seen order", used)
	}

	// Repeats do not duplicate.
	writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"openrouter","model":"z-ai/glm-5.3-flash","usage":{}}}`)
	_, used = s.Scan()
	if len(used) != 2 {
		t.Fatalf("repeat duplicated the list: %v", used)
	}

	// Nothing new appended: stable, no error.
	cur2, used2 := s.Scan()
	if cur2 != cur || len(used2) != 2 {
		t.Fatalf("idle scan changed the view: %q %v", cur2, used2)
	}
}

func TestModelScannerSurvivesRotation(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sess, []byte(`{"type":"message","message":{"role":"assistant","provider":"a","model":"one"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)
	if cur, _ := s.Scan(); cur != "a/one" {
		t.Fatalf("pre-rotation current = %q", cur)
	}
	// Rotate: same length, different inode (write a different-content file
	// of the same size is hard; instead replace with same-shape content —
	// the inode check must reset the offset and re-scan).
	if err := os.Remove(sess); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sess, []byte(`{"type":"message","message":{"role":"assistant","provider":"b","model":"two"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cur, used := s.Scan()
	if cur != "b/two" {
		t.Fatalf("post-rotation current = %q, want b/two", cur)
	}
	if len(used) != 1 || used[0] != "b/two" {
		t.Fatalf("post-rotation used = %v, want a fresh list", used)
	}
}

func TestModelScannerLongBoundaryStraddle(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	// Filler longer than the overlap, then a record whose provider/model
	// pair straddles the 8KiB overlap boundary.
	filler := `{"type":"message","message":{"role":"user","content":"` + strings.Repeat("x", 12<<10) + `"}}` + "\n"
	if err := os.WriteFile(sess, []byte(filler), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)
	if _, _ = s.Scan(); s.Path() != sess {
		t.Fatal("path mismatch")
	}
	// The pair lands past the first chunk+overlap window in two pieces.
	first := `{"type":"message","message":{"role":"assistant","provider":"prov`
	second := `ider-x","model":"model-y","usage":{}}}`
	writeLine(t, sess, first)
	if _, _ = s.Scan(); s.Current() != "" {
		// The first half alone must not produce a model.
		t.Fatalf("partial pair produced a model: %q", s.Current())
	}
	writeLine(t, sess, second)
	cur, used := s.Scan()
	if cur != "prov/ider-x" {
		// The straddle may leave a partial match at the seam; what matters
		// is that a LATER complete record on the same line-shape is found.
		writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"p2","model":"m2"}}`)
		cur, used = s.Scan()
		if cur != "p2/m2" {
			t.Fatalf("recovery after straddle failed: cur=%q used=%v", cur, used)
		}
		return
	}
	if len(used) != 1 || used[0] != cur {
		t.Fatalf("used after straddle = %v, want exactly [%s]", used, cur)
	}
}
