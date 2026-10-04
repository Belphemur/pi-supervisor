package supervisor

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// seamResolver wraps the real SessionWatcher and counts TryPath calls, so a
// test can prove the supervisor CONSULTS the watcher, not merely constructs
// it. This is the assertion a unit test cannot make: the watcher was once
// shipped with five passing unit tests while nothing in production ever
// called it — the component worked, the wiring did not exist.
type seamResolver struct {
	sessionResolver
	tryPaths *atomic.Int64
}

func (s seamResolver) TryPath() string {
	s.tryPaths.Add(1)
	return s.sessionResolver.TryPath()
}

// TestFreshLaunchAdoptsSessionViaWatcher runs a full LAUNCH round against the
// fake pi and proves the wiring, not the component:
//
//   - the round loop ARMS a SessionWatcher for the fresh-LAUNCH case
//     (sess == ""), and
//   - transcript resolution CONSULTS it (TryPath > 0) rather than only
//     polling FindSession.
//
// If the supervisor ever reverts to pure FindSession polling — the exact
// regression commit 670771e claimed to fix — both counters drop to zero and
// this test fails.
func TestFreshLaunchAdoptsSessionViaWatcher(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	report := filepath.Join(dir, "final.md")
	if err := os.WriteFile(brief, []byte("TEST_MKSESSION"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "watchwire", Brief: brief, Worktree: t.TempDir(), SessionName: "watchwire",
		Marker: "TEST_MKSESSION_MARKER", FinalReport: report,
		MaxRounds: 1, TimeoutS: 20, PiBin: pi, BackoffScale: 0.02,
	}
	writeJob(t, j)

	var armed, tryPaths atomic.Int64
	orig := newSessionResolver
	newSessionResolver = func(wt string, notBefore time.Time) (sessionResolver, error) {
		w, err := orig(wt, notBefore)
		if err != nil {
			return nil, err
		}
		armed.Add(1)
		return seamResolver{sessionResolver: w, tryPaths: &tryPaths}, nil
	}
	defer func() { newSessionResolver = orig }()

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	ch, cancel, _ := s.Watch("watchwire")
	defer cancel()
	if err := s.Start("watchwire"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, func() bool {
		st, err := job.LoadState("watchwire")
		return err == nil && st.State == "done"
	})

	if got := armed.Load(); got != 1 {
		t.Fatalf("fresh LAUNCH armed %d session watchers, want 1 — the supervisor stopped using the watcher", got)
	}
	if got := tryPaths.Load(); got < 1 {
		t.Fatalf("session resolution consulted the watcher %d times, want >= 1 — the supervisor is polling instead", got)
	}

	// And the adopted transcript is the one the fake pi created in the
	// worktree's munged sessions dir — not some other run's file.
	r := s.jobs["watchwire"]
	r.mu.Lock()
	sess := r.job.SessionPath
	r.mu.Unlock()
	if sess == "" {
		t.Fatal("session path was never pinned")
	}
	if want := job.MungedSessionsDir(j.Worktree); filepath.Dir(sess) != want {
		t.Fatalf("adopted %q, want a transcript under %q", sess, want)
	}
	if filepath.Ext(sess) != ".jsonl" {
		t.Fatalf("adopted %q, want a .jsonl transcript", sess)
	}
	// The watch channel must have carried the terminal event.
	<-ch
}
