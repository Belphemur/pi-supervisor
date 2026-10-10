package stall

// Chunk-boundary coverage (qodo PR#11 finding 8): the earlier tests used
// 12KiB fillers against a 256KiB chunk, so the multi-chunk backlog and the
// >8MiB partial-drop paths were never exercised.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelScannerMultiChunkBacklogDrainedByCallSite(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	// > 2 chunks of records: 300 records x ~1.2KiB = ~360KiB.
	var b strings.Builder
	models := []string{}
	for i := range 300 {
		m := "model-" + string(rune('a'+i%26)) + "-" + string(rune('a'+i/26))
		models = append(models, "prov/"+m)
		b.WriteString(`{"type":"message","message":{"role":"assistant","provider":"prov","model":"` + m + `"}}` + "\n")
	}
	if err := os.WriteFile(sess, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)
	// The call-site contract (supervisor drain loop): scan until idle.
	for range 100 {
		cur, used := s.Scan()
		if len(used) == len(models) && cur == models[len(models)-1] {
			break
		}
	}
	cur, used := s.Scan()
	// Record 300 (1-based) = index 299: 299%26=13 → 'n', 299/26=11 → 'l'.
	if cur != "prov/model-n-l" { //nolint:dupl // expectation spelled out on purpose
		t.Fatalf("drain missed the tail: cur=%q used=%d", cur, len(used))
	}
	if len(used) != len(models) {
		t.Fatalf("drain incomplete: %d/%d models", len(used), len(models))
	}
}

func TestModelScannerOversizedPartialDropped(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	// A single >8MiB line (no newline): the partial cap drops it.
	huge := `{"type":"message","message":{"role":"assistant","provider":"p","model":"m","pad":"` +
		strings.Repeat("x", 9<<20) + `"}}`
	if err := os.WriteFile(sess, []byte(huge), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewModelScanner(sess)
	cur, used := s.Scan()
	if cur != "" || len(used) != 0 {
		t.Fatalf("oversized partial produced a model: cur=%q used=%v", cur, used)
	}
	// The scanner must still work for the NEXT record appended after the
	// drop — and the call site must DRAIN (the 9MiB backlog takes ~36
	// chunked scans to walk past before the new record is reachable).
	f, err := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n" + `{"type":"message","message":{"role":"assistant","provider":"q","model":"after"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for range 200 {
		cur, used := s.Scan()
		if cur == "q/after" && len(used) == 1 {
			return // drained through the dropped line to the new record
		}
	}
	cur, used = s.Scan()
	t.Fatalf("post-drop drain never reached the new record: cur=%q used=%v", cur, used)
}
