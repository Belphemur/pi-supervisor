package supervisor

// ADR-0014 integration tests: the connected path from the client's TaskUpdate
// execution stream through the supervisor's single emit funnel to the event
// stream the watch CLI renders. The fixtures reuse the fake pi's
// TEST_TASKWATCH* modes (one eligible completion of task 7, subject "Write
// report") and a plugin-shaped task store in the worktree.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi-supervisor/internal/events"
	"pi-supervisor/internal/job"
)

// taskFixture writes the plugin-shaped store the fake pi's get_state identity
// (fake-sess-1) resolves to under session scope: <worktree>/.pi/tasks/.
func taskFixture(t *testing.T, worktree, store string) {
	t.Helper()
	path := filepath.Join(worktree, ".pi", "tasks", "tasks-fake-sess-1.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(store), 0o644); err != nil {
		t.Fatal(err)
	}
}

const taskStore7 = `{"nextId": 8, "tasks": [{"id": "7", "subject": "Write report",
  "description": "Summarize the campaign findings", "status": "completed",
  "activeForm": "Writing", "owner": "hermes", "metadata": {"big": "payload"},
  "blocks": [], "blockedBy": [], "createdAt": 1000, "updatedAt": 2000}]}`

// taskWatchJob wires a one-round job against the fake pi's TEST_TASKWATCH mode
// with a pinned session (resume path — the same client entry point reviews use).
func taskWatchJob(t *testing.T, prompt string) job.Job {
	t.Helper()
	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "twjob", Brief: brief, Worktree: dir, SessionName: "twjob",
		MaxRounds: 2, TimeoutS: 30, PiBin: fakePiPath(t), BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	return j
}

// waitForEvent drains ch until the named event arrives (or fails).
func waitForEvent(t *testing.T, ch <-chan events.Event, name string, budget time.Duration) events.Event {
	t.Helper()
	deadline := time.After(budget)
	for {
		select {
		case e := <-ch:
			if e.Event == name {
				return e
			}
		case <-deadline:
			t.Fatalf("event %q not observed within %s", name, budget)
		}
	}
}

// The connected happy path: an eligible TaskUpdate completion enriched from
// the plugin JSON arrives as a NON-terminal task_completed with the store's
// own fields (no title cache, no parsed prose).
func TestTaskCompletionPublished(t *testing.T) {
	testEnv(t)
	j := taskWatchJob(t, "TEST_TASKWATCH")
	taskFixture(t, j.Worktree, taskStore7)
	writeJob(t, j)

	s := newTestSupervisor(t)
	ch, cancel, pre := s.Watch("twjob")
	if pre != nil {
		t.Fatalf("precheck on a fresh job: %+v", *pre)
	}
	defer cancel()
	if err := s.Start("twjob"); err != nil {
		t.Fatal(err)
	}

	ev := waitForEvent(t, ch, "task_completed", 45*time.Second)
	if ev.Terminal() {
		t.Fatalf("task_completed must be non-terminal")
	}
	if ev.ToolCallID != "c7" || ev.TaskID != "7" || ev.Task == nil {
		t.Fatalf("event identity wrong: %+v", ev)
	}
	if ev.Task.Subject != "Write report" || ev.Task.Status != "completed" ||
		ev.Task.Description != "Summarize the campaign findings" ||
		ev.Task.Owner != "hermes" || ev.Task.UpdatedAtMS != 2000 {
		t.Fatalf("task info wrong: %+v", ev.Task)
	}
	if ev.TaskFile == "" {
		t.Fatal("task_file missing")
	}
	if ev.Round < 1 {
		t.Fatalf("round not stamped: %d", ev.Round)
	}
	if ev.Job != "twjob" {
		t.Fatalf("job not stamped: %q", ev.Job)
	}
}

// The no-store path: an eligible completion whose store is missing produces
// exactly one task_lookup_failed with a machine-readable reason, and the
// round is NOT failed by it.
func TestTaskLookupFailedPublished(t *testing.T) {
	testEnv(t)
	j := taskWatchJob(t, "TEST_TASKWATCH")
	taskFixturePath := filepath.Join(j.Worktree, ".pi", "tasks")
	_ = taskFixturePath // store deliberately NOT written
	writeJob(t, j)

	s := newTestSupervisor(t)
	ch, cancel, _ := s.Watch("twjob")
	defer cancel()
	if err := s.Start("twjob"); err != nil {
		t.Fatal(err)
	}

	ev := waitForEvent(t, ch, "task_lookup_failed", 45*time.Second)
	if ev.Terminal() {
		t.Fatalf("task_lookup_failed must be non-terminal")
	}
	if ev.TaskID != "7" || ev.ToolCallID != "c7" {
		t.Fatalf("identity wrong: %+v", ev)
	}
	if ev.Reason == "" {
		t.Fatal("task_lookup_failed needs a machine-readable reason")
	}
	// The round itself must still classify normally.
	for range 1 {
		_ = waitForEvent(t, ch, "round_done", 45*time.Second)
	}
}

// Malformed data must not fake a completion: task_lookup_failed with
// invalid-data, never task_completed.
func TestTaskLookupFailedOnBadData(t *testing.T) {
	testEnv(t)
	j := taskWatchJob(t, "TEST_TASKWATCH")
	taskFixture(t, j.Worktree, "{not json at all")
	writeJob(t, j)

	s := newTestSupervisor(t)
	ch, cancel, _ := s.Watch("twjob")
	defer cancel()
	if err := s.Start("twjob"); err != nil {
		t.Fatal(err)
	}
ev:
	for {
		select {
		case e := <-ch:
			if e.Event == "task_completed" {
				t.Fatalf("malformed store must never fake completion: %+v", e)
			}
			if e.Event == "task_lookup_failed" {
				break ev
			}
		case <-time.After(45 * time.Second):
			t.Fatal("neither task_completed nor task_lookup_failed within 45s")
		}
	}
}

// Observation ordering: the task event must be flushed BEFORE the round's
// terminal classification closes the watch (round_done arrives after).
func TestTaskEventPrecedesRoundDone(t *testing.T) {
	testEnv(t)
	j := taskWatchJob(t, "TEST_TASKWATCH")
	taskFixture(t, j.Worktree, taskStore7)
	j.MaxRounds = 1
	writeJob(t, j)

	s := newTestSupervisor(t)
	ch, cancel, _ := s.Watch("twjob")
	defer cancel()
	if err := s.Start("twjob"); err != nil {
		t.Fatal(err)
	}
	seenRoundDone := false
	deadline := time.After(45 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Event == "round_done" {
				seenRoundDone = true
			}
			if e.Event == "task_completed" && seenRoundDone {
				t.Fatal("task_completed arrived after round_done: not flushed before terminal close")
			}
			if e.Event == "task_completed" {
				return // before the terminal close, as required
			}
		case <-deadline:
			t.Fatal("task_completed never arrived")
		}
	}
}

// Both new events are pinned NON-terminal in the shared classification.
func TestTaskEventsNonTerminal(t *testing.T) {
	for _, kind := range []string{"task_completed", "task_lookup_failed"} {
		if (events.Event{Event: kind}).Terminal() {
			t.Errorf("%s must not be terminal", kind)
		}
	}
	if !(events.Event{Event: "done"}).Terminal() {
		t.Error("done must stay terminal (guard against a TEST nowise change)")
	}
}

// Two simultaneous sessions reusing the same task id #1 stay isolated: each
// job's event carries ITS OWN store's subject, never the other's.
func TestSessionIsolationForSharedTaskID(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	mk := func(name, store string) {
		dir := t.TempDir()
		brief := filepath.Join(dir, "brief.md")
		if err := os.WriteFile(brief, []byte("TEST_TASKWATCH_ALT7"), 0o644); err != nil {
			t.Fatal(err)
		}
		j := job.Job{Name: name, Brief: brief, Worktree: dir, SessionName: name,
			MaxRounds: 1, TimeoutS: 30, PiBin: pi, BackoffScale: 0.02}
		pinSession(t, &j, brief)
		path := filepath.Join(dir, ".pi", "tasks", "tasks-fake-sess-1.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(store), 0o644); err != nil {
			t.Fatal(err)
		}
		writeJob(t, j)
	}
	store := func(subject string) string {
		b, _ := json.Marshal(map[string]any{
			"nextId": 8,
			"tasks": []map[string]any{{
				"id": "7", "subject": subject, "description": "", "status": "completed",
				"blocks": []any{}, "blockedBy": []any{},
				"createdAt": 1, "updatedAt": 1,
			}},
		})
		return string(b)
	}
	mk("isoA", store("SUBJECT-A"))
	mk("isoB", store("SUBJECT-B"))

	s := newTestSupervisor(t)
	chA, cancelA, _ := s.Watch("isoA")
	defer cancelA()
	chB, cancelB, _ := s.Watch("isoB")
	defer cancelB()
	if err := s.Start("isoA"); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("isoB"); err != nil {
		t.Fatal(err)
	}
	gotA, gotB := "", ""
	deadline := time.After(60 * time.Second)
	for gotA == "" || gotB == "" {
		select {
		case e := <-chA:
			if e.Job != "isoA" {
				continue // Watch subscribes to all jobs; filter here
			}
			if e.Event == "task_completed" {
				if e.Task == nil {
					t.Fatal("A: nil task")
				}
				gotA = e.Task.Subject
			}
		case e := <-chB:
			if e.Job != "isoB" {
				continue
			}
			if e.Event == "task_completed" {
				if e.Task == nil {
					t.Fatal("B: nil task")
				}
				gotB = e.Task.Subject
			}
		case <-deadline:
			t.Fatalf("isolation test timeout: A=%q B=%q", gotA, gotB)
		}
	}
	if gotA != "SUBJECT-A" || gotB != "SUBJECT-B" {
		t.Fatalf("cross-session retarget: A=%q B=%q", gotA, gotB)
	}
}

// A stop while observations are pending must remain bounded (the funnel never
// deadlocks the stop path) and must not attribute the old round's event to a
// new round.
func TestStopDoesNotStallOnPendingObservations(t *testing.T) {
	testEnv(t)
	j := taskWatchJob(t, "TEST_TASKWATCH")
	taskFixture(t, j.Worktree, taskStore7)
	writeJob(t, j)

	s := newTestSupervisor(t)
	ch, cancel, _ := s.Watch("twjob")
	defer cancel()
	if err := s.Start("twjob"); err != nil {
		t.Fatal(err)
	}
	// Wait for task_completed (flush path exercised), then stop cleanly.
	waitForEvent(t, ch, "task_completed", 45*time.Second)
	start := time.Now()
	if err := s.Stop("twjob"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("stop blocked %s on task watch teardown", d)
	}
	// No late task event may be attributed to round 0. Bounded listen: the
	// watch channel is never closed by design, so a plain `range` would hang.
	deadline := time.After(20 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Event == "task_completed" && e.Round == 0 {
				t.Fatal("a late task event carried round 0")
			}
		case <-deadline:
			return
		}
	}
}
