package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// testEnv points the job dirs at a temp home so tests never touch the real
// ~/.pi/supervisor tree or the live campaign.
func testEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(job.JobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(job.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func fakePiPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/fake-pi.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("fake-pi.py missing: %v", err)
	}
	return p
}

// pinSession gives the job a session JSONL and a continuation prompt, which
// is what a real resumed campaign looks like. Without it the daemon's
// never-fork guard correctly refuses to LAUNCH a second time (no session dir
// in a temp worktree) and marks the job fatal after round 1.
func pinSession(t *testing.T, j *job.Job, promptFile string) {
	t.Helper()
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j.SessionPath = sess
	j.Cont = promptFile
}

func writeJob(t *testing.T, j job.Job) {
	t.Helper()
	if err := job.Save(j); err != nil {
		t.Fatal(err)
	}
}

// A job whose marker is never emitted must stop after max_rounds, not loop
// forever, and must persist fatal state.
func TestRoundCapStopsJob(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	brief := filepath.Join(t.TempDir(), "brief.md")
	// TEST_NOEND exits without agent_end -> rc 2, never the marker.
	if err := os.WriteFile(brief, []byte("TEST_NOEND"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "capped", Brief: brief, Worktree: t.TempDir(), SessionName: "capped",
		MaxRounds: 2, TimeoutS: 20, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("capped"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool {
		st, _ := s.Status("capped")
		m, _ := st.(job.Status)
		return m.State == "fatal"
	})
	st, _ := s.Status("capped")
	m, ok := st.(job.Status)
	if !ok {
		t.Fatalf("status = %#v, want a job.Status", st)
	}
	if m.Round != 2 {
		t.Fatalf("Round = %d, want 2 (the cap)", m.Round)
	}
}

// Marker + final report present in the run log ends the job as done.
func TestMarkerGateFinishesJob(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	// The fake echoes the prompt; the marker rides along in the text.
	if err := os.WriteFile(brief, []byte("TEST_STREAM MARKER_DONE"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "report.md")
	if err := os.WriteFile(report, []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "finished", Brief: brief, FinalReport: report, Marker: "MARKER_DONE",
		Worktree: t.TempDir(), SessionName: "finished",
		MaxRounds: 3, TimeoutS: 20, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("finished"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool {
		st, _ := s.Status("finished")
		m, _ := st.(job.Status)
		return m.State == "done"
	})
	st, _ := s.Status("finished")
	fin, ok := st.(job.Status)
	if !ok {
		t.Fatalf("status = %#v, want a job.Status", st)
	}
	if !fin.MarkerFound {
		t.Fatal("MarkerFound false on a done job")
	}
}

// A run that dies in under a minute with a tiny log is an instant exit and
// must accumulate strikes.
func TestInstantExitStrikesAccumulate(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_ERROR"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "striker", Brief: brief, Worktree: t.TempDir(),
		SessionName: "striker", MaxRounds: 10, TimeoutS: 20, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("striker"); err != nil {
		t.Fatal(err)
	}
	// Three strikes end the job as fatal well before the round cap.
	waitFor(t, 90*time.Second, func() bool {
		st, _ := s.Status("striker")
		m, _ := st.(job.Status)
		return m.State == "fatal"
	})
	st, _ := s.Status("striker")
	m, ok := st.(job.Status)
	if !ok {
		t.Fatalf("status = %#v, want a job.Status", st)
	}
	if m.InstantExits < 3 {
		t.Fatalf("InstantExits = %d, want >= 3", m.InstantExits)
	}
	if !strings.Contains(m.LastDiag, "instant exits") {
		t.Fatalf("LastDiag = %q, want the strike explanation", m.LastDiag)
	}
}

// RunningCount is what feeds STATUS= to systemd; it must track real rounds.
func TestRunningCountTracksActiveJobs(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	slow := filepath.Join(dir, "slow.md")
	if err := os.WriteFile(slow, []byte("TEST_SLOW SECS=6"), 0o644); err != nil {
		t.Fatal(err)
	}
	quick := filepath.Join(dir, "quick.md")
	if err := os.WriteFile(quick, []byte("TEST_STREAM"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, j := range []job.Job{
		{Name: "slowjob", Brief: slow, Worktree: t.TempDir(), SessionName: "slowjob", MaxRounds: 1, TimeoutS: 30, PiBin: pi},
		{Name: "quickjob", Brief: quick, Worktree: t.TempDir(), SessionName: "quickjob", MaxRounds: 1, TimeoutS: 30, PiBin: pi},
	} {
		writeJob(t, j)
	}

	s := newTestSupervisor(t)
	if n := s.RunningCount(); n != 0 {
		t.Fatalf("RunningCount = %d before any start, want 0", n)
	}
	if err := s.Start("slowjob"); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("quickjob"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 2 })
	// quickjob finishes fast; the count must fall back to 1.
	waitFor(t, 60*time.Second, func() bool { return s.RunningCount() == 1 })
}

// State must survive a supervisor rebuild: load, adopt, keep the round count.
func TestStateSurvivesReload(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_STREAM"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJob(t, job.Job{
		Name: "persisted", Brief: brief, Worktree: t.TempDir(),
		SessionName: "persisted", MaxRounds: 2, TimeoutS: 20, PiBin: pi,
	})

	// Simulate a daemon that already ran rounds: write state by hand.
	if err := job.SaveState("persisted", job.State{
		Round: 1, State: "running", LastRC: 0, LastDurS: 42, InstantExits: 0,
	}); err != nil {
		t.Fatal(err)
	}

	s := newTestSupervisor(t)
	st, err := s.Status("persisted")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := st.(job.Status)
	if !ok {
		t.Fatalf("status = %#v, want a job.Status", st)
	}
	if m.Round != 1 {
		t.Fatalf("Round = %d, want the persisted 1", m.Round)
	}
	// Adopted as resumable, never auto-running.
	if m.State != "stopped" {
		t.Fatalf("State = %q, want stopped after adoption", m.State)
	}
}

// Stop must end the loop and leave the job resumable, and the state file on
// disk must reflect it.
func TestStopLeavesResumableState(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=30"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJob(t, job.Job{
		Name: "stoppable", Brief: brief, Worktree: t.TempDir(),
		SessionName: "stoppable", MaxRounds: 5, TimeoutS: 60, PiBin: pi,
	})

	s := newTestSupervisor(t)
	if err := s.Start("stoppable"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })
	if err := s.Stop("stoppable"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool { return s.RunningCount() == 0 })

	// Poll the FILE, not the in-memory state: active=false is published a
	// moment before persistState() lands, so the durable write is the thing
	// worth asserting on.
	waitFor(t, 30*time.Second, func() bool {
		raw, err := os.ReadFile(filepath.Join(job.StateDir(), "stoppable.json"))
		if err != nil {
			return false
		}
		var persisted job.State
		if err := json.Unmarshal(raw, &persisted); err != nil {
			return false
		}
		return persisted.State == "stopped"
	})
	raw, err := os.ReadFile(filepath.Join(job.StateDir(), "stoppable.json"))
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	var persisted job.State
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.State != "stopped" {
		t.Fatalf("persisted State = %q, want stopped", persisted.State)
	}
	// Resumable: a fresh start must be accepted.
	if err := s.Start("stoppable"); err != nil {
		t.Fatalf("restart after stop rejected: %v", err)
	}
	// The second stop is idempotent: stopping an already-stopped job is
	// accepted, not an error.
	if err := s.Stop("stoppable"); err != nil {
		t.Fatalf("stop of an already-stopped job rejected: %v", err)
	}
}

// Steering appends to the ctrl file the client polls.
func TestSteerWritesControlFile(t *testing.T) {
	testEnv(t)
	writeJob(t, job.Job{
		Name: "steerable", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "steerable", MaxRounds: 1, TimeoutS: 20, PiBin: "true",
	})
	s := newTestSupervisor(t)
	if err := s.Steer("steerable", `{"type":"prompt","message":"POLICY CHANGE"}`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(job.Ctrl("steerable"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "POLICY CHANGE") {
		t.Fatalf("ctrl file = %q, want the steer frame", string(data))
	}
}

// Atomic state writes must never leave a partial file behind.
func TestStateWriteIsAtomic(t *testing.T) {
	testEnv(t)
	for i := range 50 {
		if err := job.SaveState("atomic", job.State{Round: i, State: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := job.LoadState("atomic")
	if err != nil {
		t.Fatal(err)
	}
	if got.Round != 49 {
		t.Fatalf("Round = %d, want 49", got.Round)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(job.StateDir())
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// The never-fork guard: a LAUNCH round with no capturable session must NOT
// re-launch a fresh session on the next round. It fails the job instead.
func TestNeverForkGuardFailsJobWithoutSession(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_STREAM"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No SessionPath and a temp worktree with no pi session dir: after
	// round 1 the daemon cannot capture a session and must refuse to fork.
	writeJob(t, job.Job{
		Name: "forker", Brief: brief, Worktree: t.TempDir(),
		SessionName: "forker", MaxRounds: 5, TimeoutS: 20, PiBin: pi, BackoffScale: 0.02,
	})

	s := newTestSupervisor(t)
	if err := s.Start("forker"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool {
		st, _ := s.Status("forker")
		m, _ := st.(job.Status)
		return m.State == "fatal"
	})
	st, _ := s.Status("forker")
	m, ok := st.(job.Status)
	if !ok {
		t.Fatalf("status = %#v, want a job.Status", st)
	}
	if m.Round != 1 {
		t.Fatalf("Round = %d, want 1 — a second LAUNCH would fork the session", m.Round)
	}
	if !strings.Contains(m.LastDiag, "fork") {
		t.Fatalf("LastDiag = %q, want the fork-refusal reason", m.LastDiag)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// stopAll tears down every running job so tests never leak pi processes.
func stopAll(s *Supervisor) {
	s.mu.Lock()
	names := make([]string, 0, len(s.jobs))
	for n := range s.jobs {
		names = append(names, n)
	}
	s.mu.Unlock()
	for _, n := range names {
		_ = s.Stop(n)
	}
	// Give each in-flight round's client goroutine a moment to observe the
	// stop channel and reap its pi process before the test exits.
	time.Sleep(time.Second)
}

// newTestSupervisor wires up a supervisor with the temp HOME already applied by
// testEnv, and arranges stopAll on test completion to guarantee no leaked
// pi/fake-pi goroutines across failures.
func newTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatalf("LoadJobs: %v", err)
	}
	t.Cleanup(func() { stopAll(s) })
	return s
}
