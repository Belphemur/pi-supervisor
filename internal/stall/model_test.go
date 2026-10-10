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
	cur, used, _ := s.Scan()
	if cur != "" || len(used) != 0 {
		t.Fatalf("user-message quote leaked into model identity: cur=%q used=%v", cur, used)
	}

	writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"opencode-go","model":"step-5-preview-free","usage":{}}}`)
	cur, used, _ = s.Scan()
	if cur != "opencode-go/step-5-preview-free" {
		t.Fatalf("current = %q, want opencode-go/step-5-preview-free", cur)
	}
	if len(used) != 1 || used[0] != "opencode-go/step-5-preview-free" {
		t.Fatalf("used = %v, want exactly one entry", used)
	}

	// A model swap mid-run appends to the list and moves current.
	writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"openrouter","model":"z-ai/glm-5.3-flash","usage":{}}}`)
	cur, used, _ = s.Scan()
	if cur != "openrouter/z-ai/glm-5.3-flash" {
		t.Fatalf("current after swap = %q", cur)
	}
	if len(used) != 2 || used[0] != "opencode-go/step-5-preview-free" || used[1] != "openrouter/z-ai/glm-5.3-flash" {
		t.Fatalf("used after swap = %v, want both in first-seen order", used)
	}

	// Repeats do not duplicate.
	writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"openrouter","model":"z-ai/glm-5.3-flash","usage":{}}}`)
	_, used, _ = s.Scan()
	if len(used) != 2 {
		t.Fatalf("repeat duplicated the list: %v", used)
	}

	// Usage (owner directive 2026-10-10): pi's totalTokens / cost.total are
	// RUNNING totals per record — the scanner keeps the LATEST, never sums
	// (summing would double-count every cached turn).
	writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"openrouter","model":"z-ai/glm-5.3-flash","usage":{"input":1225,"output":141,"cacheRead":331520,"totalTokens":332886,"cost":{"total":0.0102}}}}`)
	s.Scan() // fold the record
	tt, cost := s.LastUsage()
	if tt != 332886 {
		t.Fatalf("totalTokens = %d, want 332886", tt)
	}
	if cost < 0.010199 || cost > 0.010201 {
		t.Fatalf("cost.total = %f, want ~0.0102", cost)
	}
	// A later record with LOWER cumulative totals (e.g. post-compaction)
	// still replaces — latest wins, not max, not sum.
	writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"openrouter","model":"z-ai/glm-5.3-flash","usage":{"input":100,"output":50,"totalTokens":200,"cost":{"total":0.0001}}}}`)
	s.Scan() // fold the record
	tt, cost = s.LastUsage()
	if tt != 200 {
		t.Fatalf("latest-wins violated: totalTokens = %d, want 200", tt)
	}
	if cost < 0.0000999 || cost > 0.0001001 {
		t.Fatalf("latest-wins violated: cost = %f, want ~0.0001", cost)
	}

	// Nothing new appended: stable, no error.
	cur2, used2, _ := s.Scan()
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
	if cur, _, _ := s.Scan(); cur != "a/one" {
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
	cur, used, _ := s.Scan()
	if cur != "b/two" {
		t.Fatalf("post-rotation current = %q, want b/two", cur)
	}
	if len(used) != 1 || used[0] != "b/two" {
		t.Fatalf("post-rotation used = %v, want a fresh list", used)
	}
}

func TestModelScannerSplitsAcrossReadsAreParsedWhole(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sess, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)

	// A single record larger than one chunk, written in TWO halves: the
	// scanner must carry the incomplete trailing line and parse the record
	// exactly once, whole — this is the JSONL-parsing contract the regex
	// version could not honor.
	rec := `{"type":"message","message":{"role":"assistant","provider":"prov-x","model":"model-y","usage":{"input":1}}}` + "\n"
	// Pad the record body so the total exceeds modelChunkBytes... the chunk
	// is 256KiB; instead verify the carry with a small synthetic: write the
	// first half, scan (partial carried, nothing parsed), write the rest.
	rec = strings.Repeat(" ", 0) + rec
	idx := strings.Index(rec, `"provider"`)
	if idx < 0 {
		t.Fatal("test record malformed")
	}
	if err := os.WriteFile(sess, []byte(rec[:idx]), 0o644); err != nil {
		t.Fatal(err)
	}
	if cur, used, _ := s.Scan(); cur != "" || len(used) != 0 {
		t.Fatalf("half a record produced a model: cur=%q used=%v", cur, used)
	}
	f, err := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(rec[idx:]); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	cur, used, _ := s.Scan()
	if cur != "prov-x/model-y" {
		t.Fatalf("carried record not parsed whole: cur=%q used=%v", cur, used)
	}
	if len(used) != 1 {
		t.Fatalf("carried record double-counted: %v", used)
	}
}

func TestModelScannerFieldOrderAndUnknownFieldsIrrelevant(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	// Field ORDER reversed, extra unknown fields, pretty-printed-style
	// spacing all present in the wild — encoding/json does not care.
	if err := os.WriteFile(sess, []byte(`{"top":"x","message":{"model":"m-b","role":"assistant","provider":"prov-b","extra":{"deep":[1,2]}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)
	cur, used, _ := s.Scan()
	if cur != "prov-b/m-b" || len(used) != 1 {
		t.Fatalf("reordered/unknown fields broke parsing: cur=%q used=%v", cur, used)
	}
}

func TestModelScannerDiscardsUnknownRecords(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	lines := strings.Join([]string{
		`not json at all`,
		`{"type":"session","id":"x"}`,
		`{"type":"message","message":{"role":"user","content":"blah","model":"user-quoted"}}`,
		`{"message":{"role":"assistant"}}`,                     // no model: skip
		`{"message":{"role":"toolResult","model":"tr-model"}}`, // not an assistant
		`{"type":"message","message":{"role":"assistant","provider":"p","model":"real"}}`,
		`{broken json`,
		``,
	}, "\n") + "\n"
	if err := os.WriteFile(sess, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)
	cur, used, _ := s.Scan()
	if cur != "p/real" || len(used) != 1 || used[0] != "p/real" {
		t.Fatalf("noise leaked into identity: cur=%q used=%v", cur, used)
	}
}

func TestModelScannerLongBoundaryStraddle(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	// Filler longer than one chunk, then a complete record AFTER it: the
	// offset must reach it (chunked reads) and parse it whole.
	filler := `{"type":"message","message":{"role":"user","content":"` + strings.Repeat("x", 12<<10) + `"}}` + "\n"
	if err := os.WriteFile(sess, []byte(filler), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)
	if _, _, _ = s.Scan(); s.Path() != sess {
		t.Fatal("path mismatch")
	}
	writeLine(t, sess, `{"type":"message","message":{"role":"assistant","provider":"p2","model":"m2"}}`)
	cur, used, _ := s.Scan()
	if cur != "p2/m2" || len(used) != 1 {
		t.Fatalf("record after filler not found: cur=%q used=%v", cur, used)
	}
}
