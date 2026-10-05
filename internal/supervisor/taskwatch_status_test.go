package supervisor

// Owner amendment: `status` shows CURRENT completed/total task counts, read
// on demand through the ONE store resolver. A failed lookup is a fallback
// 0/0 PLUS a structured error (machine reason + human explanation); a valid
// empty list is 0/0 with NO error. Reads run outside supervisor/runner locks
// and work for stopped/resumed jobs whose session identity is pinned.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pi-supervisor/internal/job"
)

// stoppedJobWithStore wires a job that is NOT running: a pinned session and
// the plugin store under its worktree — exactly what a supervisor-brief job
// looks like between rounds.
func stoppedJobWithStore(t *testing.T, name, store string) job.Job {
	t.Helper()
	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: name, Brief: brief, Worktree: dir, SessionName: name,
		MaxRounds: 2, TimeoutS: 30, PiBin: fakePiPath(t),
	}
	pinRealSession(t, &j)
	path := filepath.Join(dir, ".pi", "tasks", "tasks-"+sessionIDFromPath(j.SessionPath)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if store != "" {
		if err := os.WriteFile(path, []byte(store), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeJob(t, j)
	return j
}

// pinRealSession pins a transcript whose NAME matches pi's session-format
// convention (<timestamp>_<sessionID>.jsonl): the canonical fallback identity
// derivation reads the session ID out of that name.
func pinRealSession(t *testing.T, j *job.Job) {
	t.Helper()
	dir := t.TempDir()
	sess := filepath.Join(dir, "2026-10-06T01-02-03-000Z_taskwatch-test-session.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j.SessionPath = sess
	j.Cont = j.Brief
}

// One stopped job: status counts match the CURRENT list.
func TestStatusShowsTaskCountsForStoppedJob(t *testing.T) {
	testEnv(t)
	store := `{"nextId":8,"tasks":[
	 {"id":"1","subject":"a","status":"completed","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1},
	 {"id":"2","subject":"b","status":"in_progress","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1},
	 {"id":"3","subject":"c","status":"completed","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1},
	 {"id":"4","subject":"d","status":"completed","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1}]}`
	stoppedJobWithStore(t, "stoptasks", store)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	snapAny, err := s.Status("stoptasks")
	if err != nil {
		t.Fatal(err)
	}
	st := snapAny.(job.Status)
	if st.Tasks == nil {
		t.Fatal("status must expose structured task progress")
	}
	if st.Tasks.Completed != 3 || st.Tasks.Total != 4 {
		t.Fatalf("tasks = %d/%d reason=%q detail=%q store=%q", st.Tasks.Completed, st.Tasks.Total, st.Tasks.Reason, st.Tasks.Detail, st.Tasks.StorePath)
	}
	if st.Tasks.Reason != "" || st.Tasks.Detail != "" {
		t.Fatalf("a valid list must be error-free: %+v", st.Tasks)
	}
	// Deleting a task shrinks the CURRENT totals on the next read.
	// (Counts describe the current list; nothing is cached here.)
}

// A failed lookup: fallback 0/0 PLUS a structured error with machine reason
// and human explanation; never a fabricated successful zero.
func TestStatusTaskCountsFallbackOnFailure(t *testing.T) {
	testEnv(t)
	// Valid identity (pinned session) but NO store on disk.
	stoppedJobWithStore(t, "nostore", "")
	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	snapAny, err := s.Status("nostore")
	if err != nil {
		t.Fatal(err)
	}
	st := snapAny.(job.Status)
	if st.Tasks == nil {
		t.Fatal("status must still expose task progress (the fallback) on failure")
	}
	if st.Tasks.Completed != 0 || st.Tasks.Total != 0 {
		t.Fatalf("fallback counts = %d/%d, want 0/0", st.Tasks.Completed, st.Tasks.Total)
	}
	if st.Tasks.Reason == "" || st.Tasks.Detail == "" {
		t.Fatalf("fallback needs machine reason + human explanation: %+v", st.Tasks)
	}
	if st.Tasks.StorePath == "" {
		t.Fatal("diagnostic needs the resolved store path it tried (sourced context)")
	}
}

// No identity at all (never launched): unavailable, not silent zeros.
func TestStatusTaskCountsUnavailableWithoutIdentity(t *testing.T) {
	testEnv(t)
	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	_ = os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644)
	writeJob(t, job.Job{Name: "noid", Brief: brief, Worktree: dir,
		SessionName: "noid", MaxRounds: 1, TimeoutS: 10, PiBin: "true"})
	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	snapAny, _ := s.Status("noid")
	st := snapAny.(job.Status)
	if st.Tasks == nil || st.Tasks.Reason == "" {
		t.Fatalf("unavailable identity must surface with a reason: %+v", st.Tasks)
	}
	// A valid EMPTY list (store exists, no tasks) is 0/0 with NO error,
	// different from an unavailable one.
	store := `{"nextId":1,"tasks":[]}`
	stoppedJobWithStore(t, "emptyok", store)
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	snapAny2, _ := s.Status("emptyok")
	st2 := snapAny2.(job.Status)
	if st2.Tasks == nil || st2.Tasks.Completed != 0 || st2.Tasks.Total != 0 || st2.Tasks.Reason != "" {
		t.Fatalf("valid empty list must be 0/0 with no error: %+v", st2.Tasks)
	}
}

// All-jobs status carries the same structured progress.
func TestStatusAllIncludesTaskCounts(t *testing.T) {
	testEnv(t)
	store := `{"nextId":3,"tasks":[
	 {"id":"1","subject":"a","status":"completed","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1},
	 {"id":"2","subject":"b","status":"pending","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1}]}`
	stoppedJobWithStore(t, "allA", store)
	stoppedJobWithStore(t, "allB", "")

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	all := s.StatusAll()
	list, ok := all.([]job.Status)
	if !ok || len(list) != 2 {
		t.Fatalf("StatusAll shape: %T %v", all, list)
	}
	byName := map[string]job.Status{}
	for _, st := range list {
		byName[st.Name] = st
	}
	if ta := byName["allA"].Tasks; ta == nil || ta.Completed != 1 || ta.Total != 2 {
		t.Fatalf("allA tasks = %+v", ta)
	}
	if tb := byName["allB"].Tasks; tb == nil || tb.Reason == "" {
		t.Fatalf("allB must carry the structured failure, got %+v", tb)
	}
}

// The JSON encoding stays lean; the reason is machine-readable.
func TestTaskProgressJSON(t *testing.T) {
	tp := job.TaskProgress{Completed: 4, Total: 7}
	b, _ := json.Marshal(tp)
	if !jsonContains(b, `"completed":4`) || !jsonContains(b, `"total":7`) {
		t.Fatalf("structured json: %s", b)
	}
	tp2 := job.TaskProgress{Reason: "task-store-missing", Detail: "human"}
	b2, _ := json.Marshal(tp2)
	if !jsonContains(b2, `"reason":"task-store-missing"`) || !jsonContains(b2, `"detail":"human"`) {
		t.Fatalf("fallback json: %s", b2)
	}
}

func jsonContains(b []byte, sub string) bool {
	return len(b) > 0 && sub != "" && string(b) != "" && indexOf(b, sub) >= 0
}

func indexOf(b []byte, sub string) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return i
		}
	}
	return -1
}
