package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TailLines returns the last n complete lines, each capped at maxChars with a
// leading ellipsis, reading only a bounded window of the file.
func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	long := strings.Repeat("x", 1000)
	content := "line1\nline2\n" + long + "\nline4\n\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := TailLines(path, 2, 300)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(got), got)
	}
	// The long line is truncated to 300 bytes with an ellipsis prefix.
	if !strings.HasPrefix(got[0], "…") || len(got[0]) != 3+300 {
		t.Fatalf("long line not truncated: len=%d prefix=%q", len(got[0]), got[0][:1])
	}
	if got[1] != "line4" {
		t.Fatalf("last line = %q, want line4", got[1])
	}

	// Small file: returns all lines (blank trailing line dropped), untruncated.
	got = TailLines(path, 10, 300)
	if len(got) != 4 || got[0] != "line1" || got[1] != "line2" || got[3] != "line4" {
		t.Fatalf("small file tail wrong: %q", got)
	}
	if !strings.HasPrefix(got[2], "…") {
		t.Fatalf("long line should stay truncated: %q", got[2][:1])
	}
}

// A missing or empty transcript is nil, not an error.
func TestTailLinesMissing(t *testing.T) {
	if got := TailLines(filepath.Join(t.TempDir(), "nope.jsonl"), 2, 300); got != nil {
		t.Fatalf("missing file: %q", got)
	}
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TailLines(empty, 2, 300); got != nil {
		t.Fatalf("empty file: %q", got)
	}
}
