package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/events"
	"pi-supervisor/internal/job"
)

// janitorFixture writes a terminal def+state pair the janitor can consider.
// ended of zero time means "no EndedAt stamp" (mtime-fallback territory);
// mtime, when non-zero, backdates the state file for that fallback case.
func janitorFixture(t *testing.T, name, state string, ended, mtime time.Time) {
	t.Helper()
	saveDef(t, job.Job{
		Name: name, Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: name, MaxRounds: 2, TimeoutS: 30, PiBin: "true",
	})
	st := job.State{State: state}
	if !ended.IsZero() {
		st.EndedAt = ended.UTC().Format(time.RFC3339)
	}
	if err := job.SaveState(name, st); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(filepath.Join(job.StateDir(), name+".json"), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// janitorSweepSupervisor is a supervisor whose jobs dir is the temp HOME,
// with the janitor enabled and the archive threshold set from t (so a test
// can prove the threshold is honored).
func janitorSweepSupervisor(t *testing.T, archiveAfter time.Duration) *Supervisor {
	t.Helper()
	s := newTestSupervisor(t)
	s.SetJanitorConfig(true, time.Hour, archiveAfter)
	return s
}

// saveDef persists a def with the same canonical cont/marker defaults
// writeJob applies, so raw fixtures stay valid for LoadJobs (ADR-0024).
func saveDef(t *testing.T, j job.Job) {
	t.Helper()
	if j.Cont == "" {
		j.Cont = "/tmp/cont.txt"
	}
	if j.Marker == "" {
		j.Marker = "TEST_MARKER_DONE"
	}
	if err := job.Save(j); err != nil {
		t.Fatal(err)
	}
}
func archivedPaths() (archiveDir string) {
	return filepath.Join(job.JobsDir(), "_archive")
}

func readArchivedBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(archivedPaths(), name+".json"))
	if err != nil {
		t.Fatalf("read archived %s: %v", name, err)
	}
	return data
}

// Every persisted terminal state archives; stopped and a young terminal job
// survive the same sweep (ADR-0025 §2). The in-memory runner is dropped from
// the map by the post-batch LoadJobs.
func TestJanitorArchivesTerminalStates(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	for _, st := range []string{"done", "fatal", "review_done", "review_exhausted"} {
		janitorFixture(t, "t_"+st, st, old, time.Time{})
	}
	janitorFixture(t, "t_stopped", "stopped", old, time.Time{})

	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.janitorSweep()

	for _, st := range []string{"done", "fatal", "review_done", "review_exhausted"} {
		name := "t_" + st
		if b := readArchivedBytes(t, "t_"+st); len(b) == 0 {
			t.Fatalf("%s: archived def empty", st)
		}
		if _, err := os.Stat(filepath.Join(job.JobsDir(), name+".json")); !os.IsNotExist(err) {
			t.Fatalf("%s def must be gone from jobs/, err=%v", name, err)
		}
		s.mu.Lock()
		_, live := s.jobs[name]
		s.mu.Unlock()
		if live {
			t.Fatalf("%s runner must be dropped from the map after archive", name)
		}
	}
	// stopped is never eligible (watch-driven resume substrate, ADR-0017).
	if _, err := os.Stat(filepath.Join(job.JobsDir(), "t_stopped.json")); err != nil {
		t.Fatalf("stopped def must stay in jobs/: %v", err)
	}
	s.mu.Lock()
	_, live := s.jobs["t_stopped"]
	s.mu.Unlock()
	if !live {
		t.Fatal("a stopped job must survive the sweep in the map")
	}
}

// janitor_archive_after is honored: a done job younger than the threshold
// is NOT archived.
func TestJanitorArchiveAfterHonored(t *testing.T) {
	testEnv(t)
	janitorFixture(t, "young", "done", time.Now().Add(-1*time.Hour), time.Time{})
	s := janitorSweepSupervisor(t, 168*time.Hour)
	s.janitorSweep()
	if _, err := os.Stat(filepath.Join(job.JobsDir(), "young.json")); err != nil {
		t.Fatalf("young done job must survive janitor_archive_after: %v", err)
	}
}

// EndedAt empty → mtime fallback: a done state whose file was last written
// 8 days ago (its terminal write) archives at the default-like threshold.
func TestJanitorMtimeFallback(t *testing.T) {
	testEnv(t)
	janitorFixture(t, "nominal", "done", time.Time{}, time.Now().Add(-8*24*time.Hour))
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.janitorSweep()
	if _, err := os.Stat(filepath.Join(archivedPaths(), "nominal.json")); err != nil {
		t.Fatalf("terminal state with no EndedAt must archive on mtime: %v", err)
	}
	// And a RECENT mtime keeps a no-stamp state alive (the fallback must not
	// treat "no anchor" as "ancient").
	testEnv(t)
	janitorFixture(t, "freshmtime", "done", time.Time{}, time.Now())
	// Chtimes already ran through janitorFixture's mtime path; re-pinning it
	// explicitly is belt-and-braces for the fresh mtime the assertion needs,
	// and a failure here would show up as a wrong archive outcome below.
	_ = os.Chtimes(filepath.Join(job.StateDir(), "freshmtime.json"), time.Now(), time.Now())
	s2 := janitorSweepSupervisor(t, 24*time.Hour)
	s2.janitorSweep()
	if _, err := os.Stat(filepath.Join(job.JobsDir(), "freshmtime.json")); err != nil {
		t.Fatalf("recent mtime must keep the job: %v", err)
	}
}

// A live runner (active) and an armed campaign (r.review != nil) are skipped
// for the sweep (ADR-0025 §3, ADR-0016 re-entry) regardless of age.
func TestJanitorSkipsLiveAndArmed(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "live", "done", old, time.Time{})
	janitorFixture(t, "armed", "done", old, time.Time{})
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.mu.Lock()
	s.jobs["live"].mu.Lock()
	s.jobs["live"].active = true
	s.jobs["live"].mu.Unlock()
	s.jobs["armed"].mu.Lock()
	s.jobs["armed"].review = &reviewCampaign{}
	s.jobs["armed"].mu.Unlock()
	s.mu.Unlock()
	s.janitorSweep()
	for _, name := range []string{"live", "armed"} {
		if _, err := os.Stat(filepath.Join(job.JobsDir(), name+".json")); err != nil {
			t.Fatalf("%s must survive the sweep: %v", name, err)
		}
		s.mu.Lock()
		_, ok := s.jobs[name]
		s.mu.Unlock()
		if !ok {
			t.Fatalf("%s must remain in the map", name)
		}
	}
}

// A re-used job name colliding in _archive/ gains the timestamp suffix (plus
// a counter on same-second collision); both bytes stay intact and the
// original _archive/<name>.json is never overwritten (ADR-0025 §5).
func TestJanitorDefCollisionSuffix(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "dupe", "done", old, time.Time{})
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.janitorSweep()
	first := readArchivedBytes(t, "dupe")
	// Re-create the def under the same name with different content and a
	// fresh old state; the candidate must land BESIDE the first archive.
	saveDef(t, job.Job{
		Name: "dupe", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "dupe", MaxRounds: 2, TimeoutS: 30, PiBin: "true",
	})
	if err := job.SaveState("dupe", job.State{State: "fatal", EndedAt: old.UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	s2 := janitorSweepSupervisor(t, 24*time.Hour)
	s2.janitorSweep()

	// _archive/dupe.json is untouched (byte-identical to the FIRST def's
	// bytes read right after sweep one) and the re-armed candidate landed
	// BESIDE it under the timestamp suffix.
	if second := readArchivedBytes(t, "dupe"); string(first) != string(second) {
		t.Fatal("_archive/dupe.json was overwritten")
	}
	entries, err := os.ReadDir(archivedPaths())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	var collides []string
	for _, n := range names {
		if strings.HasPrefix(n, "dupe_") || n == "dupe.json" {
			collides = append(collides, n)
		}
	}
	if len(collides) != 2 {
		t.Fatalf("want dupe.json + one suffixed copy, got %v", collides)
	}
	// The suffixed copy carries the NEW def's bytes, distinct from the
	// original archive (both preserved, neither overwritten).
	suffixed := collides[0]
	if suffixed == "dupe.json" {
		suffixed = collides[1]
	}
	secondBytes, err := os.ReadFile(filepath.Join(archivedPaths(), suffixed))
	if err != nil {
		t.Fatal(err)
	}
	if string(secondBytes) == string(first) {
		t.Fatal("suffixed copy must hold the re-armed candidate's own bytes")
	}
}

// The session JSONL goes with the def: quarantined into _archived-stale/
// beside its sessions dir; a missing session file is a no-op, not an error.
func TestJanitorSessionQuarantine(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	sessDir := t.TempDir()
	sess := filepath.Join(sessDir, "withsess.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveDef(t, job.Job{
		Name: "withsess", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "withsess", MaxRounds: 2, TimeoutS: 30, PiBin: "true",
		SessionPath: sess,
	})
	if err := job.SaveState("withsess", job.State{State: "done", EndedAt: old.UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	// gonesess: session path points nowhere — the archive still lands.
	saveDef(t, job.Job{
		Name: "gonesess", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "gonesess", MaxRounds: 2, TimeoutS: 30, PiBin: "true",
		SessionPath: filepath.Join(t.TempDir(), "absent.jsonl"),
	})
	if err := job.SaveState("gonesess", job.State{State: "done", EndedAt: old.UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}

	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.janitorSweep()

	by, err := os.ReadDir(filepath.Join(sessDir, "_archived-stale"))
	if err != nil || len(by) == 0 {
		t.Fatalf("session must be quarantined under _archived-stale: err=%v", err)
	}
	if _, err := os.Stat(sess); !os.IsNotExist(err) {
		t.Fatalf("original session must be moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archivedPaths(), "gonesess.json")); err != nil {
		t.Fatalf("missing session must be a no-op, not a failure: %v", err)
	}
}

// job_archived flows through the emit funnel with the state=/def=/session=
// Info contract — and is NOT terminal, so events.Terminal and the watch
// exit list stay untouched (invariant 22).
func TestJanitorEmitsNonTerminalEvent(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	sess := filepath.Join(t.TempDir(), "archived.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveDef(t, job.Job{
		Name: "evented", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "evented", MaxRounds: 2, TimeoutS: 30, PiBin: "true",
		SessionPath: sess,
	})
	if err := job.SaveState("evented", job.State{State: "done", EndedAt: old.UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	subID, sub, cancel := events.Subscribe()
	defer cancel()

	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.janitorSweep()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no job_archived event within 5s")
		case e, ok := <-sub:
			if !ok {
				t.Fatal("subscription closed without job_archived")
			}
			if e.Event != "job_archived" {
				continue // benign other events (none expected here)
			}
			if e.Job != "evented" {
				t.Fatalf("event job = %q", e.Job)
			}
			if !strings.Contains(e.Info, "state=done def=") ||
				!strings.Contains(e.Info, "session=") {
				t.Fatalf("Info %q lacks the state=/def=/session= contract", e.Info)
			}
			if strings.Contains(e.Info, "session=none") {
				t.Fatalf("a quarantined session must be named, Info=%q", e.Info)
			}
			if (events.Event{Event: "job_archived"}).Terminal() {
				t.Fatal("job_archived must NOT be terminal (invariant 22)")
			}
			_ = subID
			return
		}
	}
}

// janitor_enabled=false means the goroutine exits after nothing: no sweep
// ever runs, no def moves.
func TestJanitorDisabled(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "kept", "done", old, time.Time{})
	s := newTestSupervisor(t)
	s.SetJanitorConfig(false, time.Hour, 24*time.Hour)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.Janitor(stop)
		close(done)
	}()
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a disabled janitor must exit immediately")
	}
	if _, err := os.Stat(filepath.Join(job.JobsDir(), "kept.json")); err != nil {
		t.Fatalf("disabled janitor must not move anything: %v", err)
	}
}

// A per-candidate failure (rename error) skips that candidate and the sweep
// continues to the next one — no partial-state machinery (ADR-0025 §5).
func TestJanitorPerCandidateErrorContinues(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "broken", "done", old, time.Time{})
	janitorFixture(t, "healthy", "done", old, time.Time{})

	subID, sub, cancel := events.Subscribe()
	defer cancel()

	s := janitorSweepSupervisor(t, 24*time.Hour)
	// Defeat "broken" at the rename: its def file vanishes between
	// LoadJobs and the sweep, so the runner stays a candidate but the move
	// fails (ENOENT).
	if err := os.Remove(filepath.Join(job.JobsDir(), "broken.json")); err != nil {
		t.Fatal(err)
	}
	s.janitorSweep()

	if _, err := os.Stat(filepath.Join(archivedPaths(), "healthy.json")); err != nil {
		t.Fatalf("sweep must continue past a failed candidate: %v", err)
	}
	// "broken" got no job_archived event; "healthy" got exactly one.
	archivedFor := map[string]int{}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			if archivedFor["healthy"] != 1 || archivedFor["broken"] != 0 {
				t.Fatalf("events after deadline: %+v", archivedFor)
			}
			_ = subID
			return
		case e, ok := <-sub:
			if !ok {
				if archivedFor["healthy"] != 1 || archivedFor["broken"] != 0 {
					t.Fatalf("subscription closed early: %+v", archivedFor)
				}
				return
			}
			if e.Event == "job_archived" {
				archivedFor[e.Job]++
			}
		}
	}
}

// The archived def content is byte-identical to the def it moved.
func TestJanitorDefBytesPreserved(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	j := job.Job{
		Name: "bytesy", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "bytesy", MaxRounds: 2, TimeoutS: 30, PiBin: "true",
	}
	writeJob(t, j)
	janitorFixtureState(t, "bytesy", old)
	origPath := filepath.Join(job.JobsDir(), "bytesy.json")
	origBytes, err := os.ReadFile(origPath)
	if err != nil {
		t.Fatal(err)
	}
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.janitorSweep()
	if got := readArchivedBytes(t, "bytesy"); string(origBytes) != string(got) {
		t.Fatalf("archived def bytes drift:\n orig=%s\n arch=%s", origBytes, got)
	}
}

func janitorFixtureState(t *testing.T, name string, old time.Time) {
	t.Helper()
	if err := job.SaveState(name, job.State{State: "done", EndedAt: old.UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
}
