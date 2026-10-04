package job

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFindSessionNotBeforeRejectsStale is the regression test for a bug found
// live on `restart --fresh`: a fresh LAUNCH's transcript is discovered by
// scanning the session directory, but pi creates that file asynchronously. The
// scan routinely runs BEFORE the new file exists, so it returned the newest
// PRE-EXISTING transcript instead. The caller then pinned that stale path to
// the job, and the round failed in a way that looked like a hung agent: no
// transcript growth, no completion marker (ADR-0011), and the empty-turn
// detector aborting a healthy session.
//
// Without the notBefore floor this test fails, because the stale file is the
// newest entry in the directory and would be returned.
func TestFindSessionNotBeforeRejectsStale(t *testing.T) {
	wt := t.TempDir()
	dir := MungedSessionsDir(wt)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A transcript from a much earlier run: old, and the newest thing in the
	// directory. This is exactly what the buggy scan adopted.
	stale := filepath.Join(dir, "stale.jsonl")
	if err := os.WriteFile(stale, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}

	// A floor set now (i.e. the launch) must exclude it, even though it is the
	// only candidate and even though nothing else exists.
	if got := FindSession("s", wt, time.Now()); got != "" {
		t.Fatalf("FindSession adopted a pre-launch transcript: got %q, want \"\"", got)
	}

	// The zero floor keeps the original behavior: newest wins.
	if got := FindSession("s", wt, time.Time{}); got != stale {
		t.Fatalf("zero floor: got %q, want %q", got, stale)
	}

	// Once the new transcript appears, it wins over the stale one.
	fresh := filepath.Join(dir, "fresh.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write fresh: %v", err)
	}
	if got := FindSession("s", wt, time.Now()); got != fresh {
		t.Fatalf("after fresh appears: got %q, want %q", got, fresh)
	}
}

// TestFindSessionSlackToleratesSameTickCreate guards the other edge: pi may
// create the transcript in the same filesystem tick as the spawn, so the floor
// must not reject the very file it is looking for.
func TestFindSessionSlackToleratesSameTickCreate(t *testing.T) {
	wt := t.TempDir()
	dir := MungedSessionsDir(wt)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	fresh := filepath.Join(dir, "fresh.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write fresh: %v", err)
	}

	// A floor slightly in the FUTURE (clock skew, or an FS with coarse
	// timestamps) must still find the file.
	if got := FindSession("s", wt, time.Now().Add(500*time.Millisecond)); got != fresh {
		t.Fatalf("skewed floor rejected the fresh transcript: got %q, want %q", got, fresh)
	}
}

// TestFindSessionSkipsDirs documents that quarantine's subdirectory is never a
// candidate (ADR-0010): quarantined transcripts must not be re-adopted.
func TestFindSessionSkipsDirs(t *testing.T) {
	wt := t.TempDir()
	dir := MungedSessionsDir(wt)

	if err := os.MkdirAll(filepath.Join(dir, "_archived-stale"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	archived := filepath.Join(dir, "_archived-stale", "old.jsonl")
	if err := os.WriteFile(archived, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write archived: %v", err)
	}
	now := time.Now()
	if err := os.Chtimes(archived, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if got := FindSession("s", wt, time.Time{}); got != "" {
		t.Fatalf("FindSession returned a quarantined transcript: %q", got)
	}
}
