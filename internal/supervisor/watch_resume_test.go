package supervisor

// ADR-0017: a daemon restart must be transparent to an armed watch. The
// daemon's OWN shutdown records stop source "daemon" (nobody asked the
// campaign to end); a watcher re-arming IS the intent signal, so the watch
// precheck RESUMES a daemon-stopped job instead of answering "already
// stopped". An operator stop ("operator") stays stopped — someone explicitly
// halted it.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// daemonStopJob writes a resumable (pinned-session) job and returns it.
func daemonStopJob(t *testing.T, name string) job.Job {
	t.Helper()
	pi := fakePiPath(t)
	dir := t.TempDir()
	prompt := filepath.Join(dir, "cont.txt")
	if err := os.WriteFile(prompt, []byte("TEST_HANG"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: name, Brief: prompt, Worktree: t.TempDir(), SessionName: name,
		MaxRounds: 10, TimeoutS: 20, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, prompt)
	writeJob(t, j)
	return j
}

// mustState reads the persisted state file and fails the test on error.
func mustState(t *testing.T, name string) job.State {
	t.Helper()
	st, err := job.LoadState(name)
	if err != nil {
		t.Fatalf("LoadState(%s): %v", name, err)
	}
	return st
}

// The full daemon-restart path: a running job is killed by Shutdown (source
// "daemon", persisted), a NEW daemon loads it, and a watch re-arms — the job
// must come back running, session intact.
func TestWatchResumesDaemonStoppedJob(t *testing.T) {
	testEnv(t)
	j := daemonStopJob(t, "dstop")

	s1 := newTestSupervisor(t)
	if err := s1.Start(j.Name); err != nil {
		t.Fatal(err)
	}
	// The install/restart: the daemon's own shutdown must NOT read as an
	// operator stop.
	s1.Shutdown()

	st := mustState(t, j.Name)
	if st.State != "stopped" || st.StopSource != "daemon" {
		t.Fatalf("after Shutdown: state=%q stop_source=%q, want stopped/daemon", st.State, st.StopSource)
	}

	// systemd restarts the unit: a fresh daemon loads jobs from disk.
	s2 := newTestSupervisor(t)
	ch, cancel, pre := s2.Watch(j.Name)
	defer cancel()
	if pre != nil {
		t.Fatalf("daemon-stopped job must resume on watch, got precheck %+v", pre)
	}
	// The precheck's resume path runs synchronously inside Watch: state must
	// be running (or reviewing under a campaign — none here) by return.
	if got := mustState(t, j.Name); got.State != "running" {
		t.Fatalf("after re-arming watch: state=%q, want running", got.State)
	}
	// And the watch is a live subscriber: stop the job to prove it, and
	// expect the terminal event through the channel (bounded).
	go func() { _ = s2.Stop(j.Name) }()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatal("resumed watch never delivered the stop event")
	}
}

// The control case: an OPERATOR stop records "operator" and a re-arming
// watch answers the plain precheck without resuming anything.
func TestWatchDoesNotResumeOperatorStoppedJob(t *testing.T) {
	testEnv(t)
	j := daemonStopJob(t, "ostop")

	s1 := newTestSupervisor(t)
	if err := s1.Start(j.Name); err != nil {
		t.Fatal(err)
	}
	if err := s1.Stop(j.Name); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, j.Name)
	if st.State != "stopped" || st.StopSource != "operator" {
		t.Fatalf("after Stop: state=%q stop_source=%q, want stopped/operator", st.State, st.StopSource)
	}

	s2 := newTestSupervisor(t)
	_, cancel, pre := s2.Watch(j.Name)
	defer cancel()
	if pre == nil || pre.Event != "stopped" {
		t.Fatalf("operator stop must NOT resume; precheck = %+v", pre)
	}
	if got := mustState(t, j.Name); got.State != "stopped" {
		t.Fatalf("operator-stopped job changed state to %q under watch", got.State)
	}
}
