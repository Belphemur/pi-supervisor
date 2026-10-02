package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// The full CI-stall flow (ADR-0004): a marker line appears in the session
// JSONL, the transcript goes quiet past ci_stall_idle_s, the watcher emits
// ci_stall events, and at the cap it (a) writes abort+prompt frames telling
// the agent to finish the report and (b) closes the run as fatal.
func TestCIStallCapInterruptsAndFatals(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=6"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "cistall", Brief: brief, Worktree: t.TempDir(), SessionName: "cistall",
		MaxRounds: 2, TimeoutS: 60, PiBin: pi,
		CIStallCap: 1, CIStallIdleS: 1, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief) // sets SessionPath + Cont
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	ch, cancel, _ := s.Watch("cistall")
	defer cancel()

	if err := s.Start("cistall"); err != nil {
		t.Fatal(err)
	}

	// Wait for the round to be live, then park the agent on CI. The small
	// delay lets the watcher goroutine's stall.New() snap its read offset at
	// the pre-marker file end; appending earlier would make the marker
	// "historical" and it would never arm (a race, not a feature).
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })
	time.Sleep(300 * time.Millisecond)
	r := s.jobs["cistall"]
	r.mu.Lock()
	sess := r.job.SessionPath
	r.mu.Unlock()
	f, err := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"content":"10 checks running. Let me wait for CI and the re-review to land."}` + "\n")
	_ = f.Close()

	// Expect ci_stall then fatal on the watch channel.
	sawStall, sawFatal := false, false
	deadline := time.After(30 * time.Second)
	for !sawFatal {
		select {
		case e := <-ch:
			switch e.Event {
			case "ci_stall":
				sawStall = true
				if !strings.Contains(e.Info, "1/1") {
					t.Fatalf("stall info = %q, want 1/1", e.Info)
				}
			case "fatal":
				sawFatal = true
				if !strings.Contains(e.Info, "CI review retry cap") {
					t.Fatalf("fatal info = %q", e.Info)
				}
			}
		case <-deadline:
			t.Fatalf("no fatal within 30s (stall seen: %v)", sawStall)
		}
	}
	if !sawStall {
		t.Fatal("fatal fired without a preceding ci_stall")
	}

	// The control file must carry the finish-the-report instruction.
	<-time.After(500 * time.Millisecond) // interruptWith write lands
	ctrl, err := os.ReadFile(job.Ctrl("cistall"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ctrl), `"type":"abort"`) ||
		!strings.Contains(string(ctrl), "Finish the review report NOW") {
		t.Fatalf("ctrl file missing interrupt frames: %s", ctrl)
	}

	// State: fatal with the stall counted.
	waitFor(t, 20*time.Second, func() bool {
		st, err := job.LoadState("cistall")
		return err == nil && st.State == "fatal"
	})
	st, err := job.LoadState("cistall")
	if err != nil {
		t.Fatal(err)
	}
	if st.CIStalls != 1 {
		t.Fatalf("ci_stalls = %d, want 1", st.CIStalls)
	}
}

// Below the cap the watcher only observes: no interrupt frames, no fatal.
func TestCIStallBelowCapOnlyObserves(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=5"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "cistall2", Brief: brief, Worktree: t.TempDir(), SessionName: "cistall2",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
		CIStallCap: 5, CIStallIdleS: 1,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	ch, cancel, _ := s.Watch("cistall2")
	defer cancel()
	if err := s.Start("cistall2"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })
	time.Sleep(300 * time.Millisecond) // watcher offset snap (see test 1)
	r := s.jobs["cistall2"]
	r.mu.Lock()
	sess := r.job.SessionPath
	r.mu.Unlock()
	f, err := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"content":"waiting for CI"}` + "\n")
	_ = f.Close()

	// One ci_stall is observed; at most that. The job may end by round cap
	// (its own normal terminal) — what must NOT happen is the CI-cap
	// fatal/interrupt.
	sawStall, sawCIFatal := 0, false
	deadline := time.After(60 * time.Second)
	for {
		select {
		case e := <-ch:
			switch e.Event {
			case "ci_stall":
				sawStall++
				if !strings.Contains(e.Info, "1/5") {
					t.Fatalf("stall info = %q, want 1/5", e.Info)
				}
			case "fatal":
				if strings.Contains(e.Info, "CI review retry cap") {
					sawCIFatal = true
				}
			}
		case <-deadline:
			t.Fatal("job never reached a terminal state")
		default:
			st, err := job.LoadState("cistall2")
			if err == nil && (st.State == "fatal" || st.State == "done") {
				if sawCIFatal {
					t.Fatal("CI-cap fatal fired below the cap")
				}
				if sawStall != 1 {
					t.Fatalf("ci_stall events = %d, want 1", sawStall)
				}
				ctrl, err := os.ReadFile(job.Ctrl("cistall2"))
				if err == nil && strings.Contains(string(ctrl), `"type":"abort"`) {
					t.Fatal("interrupt frames written below the cap")
				}
				st2, _ := job.LoadState("cistall2")
				if st2.CIStalls != 1 {
					t.Fatalf("ci_stalls = %d, want 1", st2.CIStalls)
				}
				return
			}
		}
	}
}
