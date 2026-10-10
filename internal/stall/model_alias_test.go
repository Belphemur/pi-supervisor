package stall

// kody PR#12: whole shared s.partial's backing array, so moving the
// trailing partial over the head corrupted the first complete record of
// the chunk (its model silently dropped). The fixture ends a chunk with a
// complete record PLUS a partial next record — the exact shape.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelScannerAliasingTailOverHead(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	// Chunk 1: complete record A + the first half of record B (no newline).
	a := `{"type":"message","message":{"role":"assistant","provider":"pA","model":"mA"}}` + "\n"
	bHead := `{"type":"message","message":{"role":"assistant","provider":"pB`
	if err := os.WriteFile(sess, []byte(a+bHead), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)
	cur, used, _ := s.Scan()
	if cur != "pA/mA" || len(used) != 1 {
		t.Fatalf("chunk1: cur=%q used=%v — record A lost", cur, used)
	}
	// Chunk 2: the REST of record B, ending with a newline.
	f, err := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`","model":"mB"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	cur, used, _ = s.Scan()
	if cur != "pB/mB" {
		t.Fatalf("chunk2: cur=%q — carried record B not parsed whole", cur)
	}
	if len(used) != 2 {
		t.Fatalf("used = %v, want both models", used)
	}
}
