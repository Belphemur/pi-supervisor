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
// survive the same sweep (ADR-0025 §2). The in-memory runner is dropped by
// the per-candidate targeted removal in the archival critical section —
// never by a bulk reload (qodo #9).
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
	if _, err := os.Stat(filepath.Join(job.StateDir(), "dupe.json")); !os.IsNotExist(err) {
		t.Fatal("archived job's own state file must move with it (qodo #3)")
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
// Info contract — and IS terminal now (ADR-0025 §6, qodo #7): the run of an
// archived job is over with the archive, so events.Terminal and the watch
// exit list BOTH name it (invariant 22's sync rule: same commit).
func TestJanitorEmitsTerminalEvent(t *testing.T) {
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
			if !(events.Event{Event: "job_archived"}).Terminal() {
				t.Fatal("job_archived must be terminal (ADR-0025 §6) — a watcher must end with the archive")
			}
			// The envelope context survives the map drop: the funnel context is
			// carried explicitly, so worktree (from the def) is still filled.
			if e.Worktree == "" {
				t.Fatal("job_archived must keep its envelope worktree context")
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
	// Defeat "broken" at the FIRST operation the archive performs: the
	// session quarantine (move ordering per qodo #2 — session before def).
	// A session dir that cannot create _archived-stale/ makes Quarantine
	// fail before the def ever moves, so the job stays fully intact and re-
	// candidateable for the next sweep.
	brokenDir := t.TempDir()
	brokenSess := filepath.Join(brokenDir, "broken.jsonl")
	if err := os.WriteFile(brokenSess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(brokenDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(brokenDir, 0o755) })
	s.mu.Lock()
	rB := s.jobs["broken"]
	rB.mu.Lock()
	rB.job.SessionPath = brokenSess
	rB.mu.Unlock()
	s.mu.Unlock()
	s.janitorSweep()

	if _, err := os.Stat(filepath.Join(archivedPaths(), "healthy.json")); err != nil {
		t.Fatalf("sweep must continue past a failed candidate: %v", err)
	}
	// "broken" got no job_archived event; "healthy" got exactly one. No part
	// of "broken" moved — quarantine-first failing strands nothing.
	if _, err := os.Stat(filepath.Join(job.JobsDir(), "broken.json")); err != nil {
		t.Fatalf("a failed candidate's def must stay: %v", err)
	}
	if _, err := os.Stat(brokenSess); err != nil {
		t.Fatal("a failed candidate's session must stay adoptable")
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

// A def that already left jobs/ before the sweep (a hand-mv archive while
// the daemon runs, or an earlier sweep's half-failure) recovers instead of
// warn-looping forever: the runner is cleaned, the session quarantined and
// the event emitted with def=already-moved. Nothing is overwritten.
func TestJanitorDefAlreadyGoneRecovers(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "halffeat", "done", old, time.Time{})
	sess := filepath.Join(t.TempDir(), "half.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.mu.Lock()
	r := s.jobs["halffeat"]
	r.mu.Lock()
	r.job.SessionPath = sess
	r.mu.Unlock()
	s.mu.Unlock()
	if err := os.Remove(filepath.Join(job.JobsDir(), "halffeat.json")); err != nil {
		t.Fatal(err)
	}

	subID, sub, cancel := events.Subscribe()
	defer cancel()
	s.janitorSweep()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no recuperated archive event within 5s")
		case e, ok := <-sub:
			if !ok {
				t.Fatal("subscription closed without the event")
			}
			if e.Event != "job_archived" || e.Job != "halffeat" {
				continue
			}
			if !strings.Contains(e.Info, "def=already-moved") {
				t.Fatalf("Info must name the pre-moved def, got %q", e.Info)
			}
			if _, err := os.Stat(sess); !os.IsNotExist(err) {
				t.Fatal("the session must still be quarantined on the recovery path")
			}
			s.mu.Lock()
			_, live := s.jobs["halffeat"]
			s.mu.Unlock()
			if live {
				t.Fatal("recovered archive must drop the runner")
			}
			// Exit the loop only after exactly ONE event arrived; a SECOND
			// job_archived would mean the sweep re-emits.
			select {
			case e2, ok2 := <-sub:
				if ok2 && e2.Event == "job_archived" {
					t.Fatal("recovery path must emit exactly one event")
				}
				_ = subID
				return
			case <-time.After(200 * time.Millisecond):
				return // no further events within a short window
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

// A watch that arrives AFTER the archive gets the terminal precheck instead
// of a subscription to something that will never emit again (qodo #7).
func TestWatchPrechecksArchivedJob(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "archivedw", "done", old, time.Time{})
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.janitorSweep()
	if _, err := os.Stat(filepath.Join(archivedPaths(), "archivedw.json")); err != nil {
		t.Fatal(err)
	}
	ch, cancel, pre := s.Watch("archivedw")
	defer cancel()
	if ch != nil {
		t.Fatal("an archived job must answer with the precheck, not a live channel")
	}
	if pre == nil || pre.Event != "job_archived" || pre.Job != "archivedw" {
		t.Fatalf("pre = %+v, want a job_archived precheck", pre)
	}
	// And the reporter the control server consults.
	if !s.Archived("archivedw") {
		t.Fatal("Archived must report the archived def")
	}
	if s.Archived("archivedw2") || s.Archived("../x") {
		t.Fatal("a def not in _archive (or an invalid name) must not report archived")
	}
}

func janitorFixtureState(t *testing.T, name string, old time.Time) {
	t.Helper()
	if err := job.SaveState(name, job.State{State: "done", EndedAt: old.UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
}

// The archived job's OWN state file moves with it into _archive/ (qodo #3;
// kody): state/<name>.json surviving made a re-used job name inherit the old
// terminal state (done + the old EndedAt) and get archived at its next
// sweep before running — or Start refusing with KindAlreadyDone. Numbers
// stay move-only: the state file's bytes land in _archive/<name>.state.json.
func TestJanitorStateFileMovesWithDef(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "ownstate", "done", old, time.Time{})
	orig, err := os.ReadFile(filepath.Join(job.StateDir(), "ownstate.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.janitorSweep()
	if _, err := os.Stat(filepath.Join(job.StateDir(), "ownstate.json")); !os.IsNotExist(err) {
		t.Fatalf("state file must leave state/: %v", err)
	}
	arch, err := os.ReadFile(filepath.Join(archivedPaths(), "ownstate.state.json"))
	if err != nil {
		t.Fatalf("state file must land in _archive/: %v", err)
	}
	if string(orig) != string(arch) {
		t.Fatalf("archived state bytes drift:\n orig=%s\n arch=%s", orig, arch)
	}
}

// A terminal job whose state still carries a review baseline is NOT a
// candidate (qodo #6): the baseline is what re-checks (explicit and
// post-push) read to notice new threads on that PR — archiving severs the
// only channel that reports them.
func TestJanitorSkipsReviewBaseline(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "tracked", "done", old, time.Time{})
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.mu.Lock()
	s.jobs["tracked"].mu.Lock()
	s.jobs["tracked"].state.ReviewBaseline = &job.ReviewBaseline{
		Owner: "o", Repo: "r", PR: 1, Head: "abc", ClosedAt: "2026-01-01T00:00:00Z",
	}
	s.jobs["tracked"].mu.Unlock()
	s.mu.Unlock()
	s.janitorSweep()
	if _, err := os.Stat(filepath.Join(job.JobsDir(), "tracked.json")); err != nil {
		t.Fatalf("baseline-tracked terminal job must survive the sweep: %v", err)
	}
	s.mu.Lock()
	_, live := s.jobs["tracked"]
	s.mu.Unlock()
	if !live {
		t.Fatal("baseline-tracked terminal job must stay in the map")
	}
}

// The no-replace emergency valve (qodo #8): an existing destination — file
// or dangling symlink, which os.Stat-based existence checks both pass or
// miss — advances the collision suffix; os.Link refuses EEXIST, nothing is
// ever replaced.
func TestJanitorMoveNeverOverwrites(t *testing.T) {
	testEnv(t)
	archiveDir := archivedPaths()
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A live def whose bytes would collide with an existing archive entry.
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "victim.json")
	if err := os.WriteFile(src, []byte("the-move\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 1: plain file at the base name.
	taken := filepath.Join(archiveDir, "victim.json")
	if err := os.WriteFile(taken, []byte("KEEP-ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 2: a dangling symlink at the first colliding name the mover computes —
	// os.Stat says absent; the pre-fix os.Rename would have replaced it.
	ts := time.Now().Format("2006-01-02T15-04-05")
	if err := os.Symlink(filepath.Join(srcDir, "does-not-matter"), filepath.Join(archiveDir, "victim_"+ts+".json")); err != nil {
		t.Fatal(err)
	}
	final, err := archiveMove(src, archiveDir, "victim.json")
	if err != nil {
		t.Fatalf("archiveMove: %v", err)
	}
	if keep, rerr := os.ReadFile(taken); rerr != nil || string(keep) != "KEEP-ME\n" {
		t.Fatalf("existing destination was replaced: %v %q", rerr, keep)
	}
	link, err := os.Lstat(filepath.Join(archiveDir, "victim_"+ts+".json"))
	if err != nil || link.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dangling symlink must stay untouched: stat=%v err=%v", link, err)
	}
	base := filepath.Base(final)
	if !strings.HasPrefix(base, "victim_") {
		t.Fatalf("the mover must advance past the taken base name: %q", base)
	}
	// The chosen destination must not be one of the occupied names — the
	// suffix walk advances, never replaces.
	if base == "victim.json" || base == "victim_"+ts+".json" {
		t.Fatalf("collision walk landed on an occupied name: %q", base)
	}
	moved, err := os.ReadFile(final)
	if err != nil || string(moved) != "the-move\n" {
		t.Fatalf("moved bytes: %v %q", err, moved)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source must leave after the move: %v", err)
	}
}

// A def name that breaks path composition is refused by Validate (ADR-0024)
// and the janitor never touches one, even if a hand-built runner manages to
// carry one (qodo #4 defense in depth).
func TestJanitorTraversalNameSkipped(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	// Only reachable via a hand-built runner: job.Validate refuses the name,
	// so LoadJobs would bounce the def loudly.
	const evil = "../escape"
	if err := job.SaveState(evil, job.State{State: "done", EndedAt: old.UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	escapePath := filepath.Join(job.StateDir(), evil+".json")
	orig, err := os.ReadFile(escapePath)
	if err != nil {
		t.Fatal(err)
	}
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.mu.Lock()
	s.jobs[evil] = &runner{job: job.Job{Name: evil, Brief: "/tmp/x.md", Worktree: t.TempDir(), SessionName: evil}}
	rEvil := s.jobs[evil]
	rEvil.mu.Lock()
	rEvil.state = job.State{State: "done", EndedAt: old.UTC().Format(time.RFC3339)}
	rEvil.mu.Unlock()
	s.mu.Unlock()
	s.janitorSweep()
	if got, rerr := os.ReadFile(escapePath); rerr != nil || string(got) != string(orig) {
		t.Fatalf("traversal state file must be untouched: err=%v byte-shift=%v", rerr, rerr == nil)
	}
	s.mu.Lock()
	_, still := s.jobs[evil]
	s.mu.Unlock()
	if !still {
		t.Fatal("an invalid-name runner must stay in the map, never archived")
	}
	// And the validator itself refuses every separator shape.
	for _, bad := range []string{"../escape", "sub/x", ".", "..", "a/../b", ""} {
		if job.ValidJobName(bad) {
			t.Fatalf("ValidJobName(%q) must be false", bad)
		}
		if err := job.Validate(job.Job{Name: bad}); err == nil {
			t.Fatalf("Validate must refuse name %q", bad)
		}
	}
	for _, fine := range []string{"mealime-tooltip", "aル_b.bin-2"} {
		if !job.ValidJobName(fine) {
			t.Fatalf("ValidJobName(%q) must be true", fine)
		}
	}
}

// Re-validation closes the TOCTOU (qodo #1, #5; kody): janitorArchive
// re-checks active/state/armed/baseline in the SAME critical section that
// moves and drops — anything changed between the sweep snapshot and the
// archive is refused and NOTHING moves.
func TestJanitorRevalidationSkipsRacedStart(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "raced", "done", old, time.Time{})
	s := janitorSweepSupervisor(t, 24*time.Hour)
	c := janitorCandidate{name: "raced", state: "done"}
	// Simulate Start() landing in the gap: the runner is active by the time
	// the archive locks.
	s.mu.Lock()
	r := s.jobs["raced"]
	r.mu.Lock()
	r.active = true
	r.mu.Unlock()
	s.mu.Unlock()
	if err := s.janitorArchive(c); err == nil {
		t.Fatal("an activated candidate must be refused")
	}
	if _, err := os.Stat(filepath.Join(job.JobsDir(), "raced.json")); err != nil {
		t.Fatalf("refused candidate's def must stay in jobs/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(job.StateDir(), "raced.json")); err != nil {
		t.Fatalf("refused candidate's state must stay in state/: %v", err)
	}
	s.mu.Lock()
	_, live := s.jobs["raced"]
	s.mu.Unlock()
	if !live {
		t.Fatal("refused candidate's runner must stay in the map")
	}

	// And a runner that vanished mid-sweep is refused the same way.
	janitorFixture(t, "gone", "done", old, time.Time{})
	s2 := janitorSweepSupervisor(t, 24*time.Hour)
	c2 := janitorCandidate{name: "gone", state: "done"}
	s2.mu.Lock()
	delete(s2.jobs, "gone")
	s2.mu.Unlock()
	if err := s2.janitorArchive(c2); err == nil {
		t.Fatal("a vanished runner must be refused, not acted on")
	}
	if _, err := os.Stat(filepath.Join(job.JobsDir(), "gone.json")); err != nil {
		t.Fatalf("vanished candidate's def must stay: %v", err)
	}
}

// The post-batch bulk reload is gone (kody / qodo #9): a live runner whose
// def on disk transiently fails to parse stays in the map — the janitor's
// removal is per successfully archived job only.
func TestJanitorTargetedRemovalKeepsLiveRunner(t *testing.T) {
	testEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	janitorFixture(t, "toarchive", "done", old, time.Time{})
	janitorFixture(t, "liver", "done", old, time.Time{})
	s := janitorSweepSupervisor(t, 24*time.Hour)
	s.mu.Lock()
	rLive := s.jobs["liver"]
	rLive.mu.Lock()
	rLive.active = true
	rLive.mu.Unlock()
	s.mu.Unlock()
	// Corrupt the live job's def ON DISK after the load: the old LoadJobs()
	// post-batch would then drop the live runner (its def fails to parse).
	if err := os.WriteFile(filepath.Join(job.JobsDir(), "liver.json"), []byte("{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.janitorSweep()
	if _, err := os.Stat(filepath.Join(archivedPaths(), "toarchive.json")); err != nil {
		t.Fatalf("the archivable candidate must archive: %v", err)
	}
	s.mu.Lock()
	_, liveKept := s.jobs["liver"]
	_, liveArchived := s.jobs["toarchive"]
	s.mu.Unlock()
	if !liveKept {
		t.Fatal("a live runner must never be dropped by the sweep — removal is per archived job")
	}
	if liveArchived {
		t.Fatal("the archived runner must be dropped from the map")
	}
	// The corrupted def stays where it is: LoadJobs' own loud ADR-0024
	// reporting remains the failing surface for it, at the next reload.
	if _, err := os.Stat(filepath.Join(job.JobsDir(), "liver.json")); err != nil {
		t.Fatal("the sweep must not touch unrelated def files")
	}
}

// The clamp before NewTicker (kody): a unitless PI_SUPERVISOR_JANITOR_
// INTERVAL=2 arrives as 2ns and must not reach time.NewTicker.
func TestJanitorIntervalClamp(t *testing.T) {
	for d, want := range map[time.Duration]time.Duration{
		0:                   janitorIntervalDefault,
		-5 * time.Minute:    janitorIntervalDefault,
		2 * time.Nanosecond: janitorIntervalFloor,
		time.Second:         janitorIntervalFloor,
		time.Minute:         time.Minute,
		2 * time.Hour:       2 * time.Hour,
	} {
		if got := janitorInterval(d); got != want {
			t.Fatalf("janitorInterval(%v) = %v, want %v", d, got, want)
		}
	}
	// The clamp runs before the ticker: a goroutine Janitor with a unitless
	// config must survive (pre-fix: 2ns ticker) and exit cleanly on stop.
	testEnv(t)
	s := newTestSupervisor(t)
	s.SetJanitorConfig(true, 2, 24*time.Hour)
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
		t.Fatal("janitor must exit promptly on stop")
	}
}
