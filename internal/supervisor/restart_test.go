package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// TestRestartQuarantinesAndClearsState is the acceptance test for ADR-0010:
// restart --fresh must quarantine the transcript into _archived-stale/, clear
// session_path, and reset the round counter — in that order, with the
// transcript bytes preserved.
func TestRestartQuarantinesAndClearsState(t *testing.T) {
	dir := t.TempDir()
	// A transcript with real content, so byte preservation is checkable.
	sess := filepath.Join(dir, "sess-abc.jsonl")
	content := []byte(`{"role":"assistant","content":"poisoned context"}` + "\n")
	if err := os.WriteFile(sess, content, 0o644); err != nil {
		t.Fatal(err)
	}

	s := New()
	// Wire a runner by hand: New() has no exported way to inject one, and
	// Restart only touches the runner's job/state fields plus the filesystem.
	r := &runner{
		job:    job.Job{Name: "t", Worktree: dir, SessionPath: sess},
		state:  job.State{Round: 7, State: "stopped"},
		stopCh: make(chan struct{}),
	}
	s.jobs["t"] = r

	// Restart with no live round: Stop() errors "not running", which Restart
	// must treat as "proceed to clear".
	if err := s.Restart("t"); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	// 1. The live transcript is gone from its original path.
	if job.Exists(sess) {
		t.Fatal("original transcript still present after quarantine")
	}
	// 2. It lives under _archived-stale/ with the bytes intact.
	qDir := filepath.Join(dir, "_archived-stale")
	entries, err := os.ReadDir(qDir)
	if err != nil {
		t.Fatalf("no quarantine dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("quarantine has %d entries, want 1", len(entries))
	}
	got, err := os.ReadFile(filepath.Join(qDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("quarantined bytes differ:\n got %q\nwant %q", got, content)
	}
	// 3. The quarantine name carries the original session id for audit.
	if !strings.HasPrefix(entries[0].Name(), "sess-abc_") {
		t.Fatalf("quarantine name %q lost the original stem", entries[0].Name())
	}
	// 4. The runner's session_path is cleared and the round counter reset.
	r.mu.Lock()
	gotSess, gotRound := r.job.SessionPath, r.state.Round
	r.mu.Unlock()
	if gotSess != "" {
		t.Fatalf("session_path = %q, want empty", gotSess)
	}
	if gotRound != 0 {
		t.Fatalf("round = %d, want 0", gotRound)
	}
}

// A second --fresh on the same job must not re-adopt the file it just
// quarantined: with session_path cleared, FindSession sees nothing in the
// live dir (the transcript is a subdirectory away) and reports no session.
func TestRestartFreshDoesNotReAdoptQuarantined(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "sess-old.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New()
	r := &runner{
		job:    job.Job{Name: "t", Worktree: dir, SessionPath: sess},
		state:  job.State{Round: 3, State: "stopped"},
		stopCh: make(chan struct{}),
	}
	s.jobs["t"] = r

	if err := s.Restart("t"); err != nil {
		t.Fatal(err)
	}
	// Discovery must find nothing: the only .jsonl is inside _archived-stale/.
	if got := job.FindSession("t", job.MungedSessionsDir(dir), time.Time{}); got != "" && got == sess {
		t.Fatal("FindSession re-adopted the quarantined transcript")
	}
}

// Restart on an unknown job is an error, not a silent no-op.
func TestRestartUnknownJobErrors(t *testing.T) {
	s := New()
	if err := s.Restart("nope"); err == nil {
		t.Fatal("Restart on unknown job returned nil")
	}
}
