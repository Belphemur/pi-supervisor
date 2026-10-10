package stall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelScannerSameSizeSameInoRewrite(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	a := `{"type":"message","message":{"role":"assistant","provider":"a","model":"one"}}` + "\n"
	b := `{"type":"message","message":{"role":"assistant","provider":"b","model":"two"}}` + "\n"
	// SAME length content, so size cannot reveal the rewrite.
	if len(a) != len(b) {
		t.Fatalf("test setup: lengths differ (%d vs %d)", len(a), len(b))
	}
	if err := os.WriteFile(sess, []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)
	if cur, _, _ := s.Scan(); cur != "a/one" {
		t.Fatalf("pre-rewrite current = %q", cur)
	}
	// Rewrite in place (truncate + write, same fd-level path) with a fresh
	// mtime bump.
	if err := os.WriteFile(sess, []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	// No mtime manipulation: the content fingerprint (head probe) is the
	// detector, so coarse timestamp granularity cannot mask the rewrite.
	// (The old Chtimes hack here masked a real bug in the mtime detector.)
	cur, used, _ := s.Scan()
	if cur != "b/two" {
		t.Fatalf("post-rewrite current = %q, want b/two (used=%v)", cur, used)
	}
}
