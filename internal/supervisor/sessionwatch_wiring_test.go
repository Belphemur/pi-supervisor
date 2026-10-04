package supervisor

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// seamResolver wraps the real SessionWatcher and counts Wait calls, so a
// test can prove the supervisor drives adoption from the EVENT side, not
// merely from a directory scan. This is the assertion a unit test cannot
// make: the watcher was once shipped with five passing unit tests while
// nothing in production ever called it — and a later revision used only the
// scan side, leaving the inotify watch opened and unused.
type seamResolver struct {
	sessionResolver
	waits *atomic.Int64
}

func (s seamResolver) Wait(timeout time.Duration) string {
	s.waits.Add(1)
	return s.sessionResolver.Wait(timeout)
}

// TestFreshLaunchAdoptsSessionViaWatcher runs a full LAUNCH round against the
// fake pi and proves the wiring, not the component:
//
//   - the round loop ARMS a SessionWatcher for the fresh-LAUNCH case
//     (sess == "") and drives adoption from the EVENT side (Wait > 0), and
//   - the transcript is pinned WHILE THE ROUND IS STILL LIVE — with a round
//     far shorter than the 2s resolution tick, only the fsnotify Wait
//     goroutine can do that; a scan-only or post-round-only supervisor fails.
//
// If the supervisor ever reverts to pure FindSession polling — the exact
// regression commit 670771e claimed to fix — the counters drop to zero and
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

	var armed, waits atomic.Int64
	orig := newSessionResolver
	newSessionResolver = func(wt string, notBefore time.Time) (sessionResolver, error) {
		w, err := orig(wt, notBefore)
		if err != nil {
			return nil, err
		}
		armed.Add(1)
		return seamResolver{sessionResolver: w, waits: &waits}, nil
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

	// The transcript must be pinned WHILE THE ROUND IS STILL LIVE. The fake
	// pi's round is far shorter than the marker watcher's 2s tick, so no
	// captureSession tick can pin mid-round: only the event-driven Wait
	// goroutine can. A tick-only or post-round-only wiring fails here.
	pinnedLive := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r := s.jobs["watchwire"]
		r.mu.Lock()
		pinned := r.job.SessionPath != ""
		active := r.active
		r.mu.Unlock()
		if pinned {
			pinnedLive = active
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	waitFor(t, 30*time.Second, func() bool {
		st, err := job.LoadState("watchwire")
		return err == nil && st.State == "done"
	})

	if got := armed.Load(); got != 1 {
		t.Fatalf("fresh LAUNCH armed %d session watchers, want 1 — the supervisor stopped using the watcher", got)
	}
	if got := waits.Load(); got < 1 {
		t.Fatalf("nothing ever called Wait (%d) — the inotify watch is opened and unused, adoption is scan-only", got)
	}
	if !pinnedLive {
		t.Fatal("session transcript was not pinned while the round was live — adoption is not event-driven (Wait never ran or fired only post-round)")
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
