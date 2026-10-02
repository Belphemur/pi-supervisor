package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/events"
	"pi-supervisor/internal/job"
	"pi-supervisor/internal/stall"
)

const realPRURL = "https://github.com/Belphemur/XPoint/pull/184"

// prFixture writes a job file plus a fake session JSONL holding only the
// round-1 seed line, and returns a loaded Supervisor with a runner for it.
// Transcript lines are appended by appendTranscript AFTER the detector is
// created — exactly like a live round, where the URL appears in content the
// detector has not read yet. Never touches the live session file.
func prFixture(t *testing.T) (*Supervisor, string) {
	t.Helper()
	testEnv(t)

	dir := t.TempDir()
	sess := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sess, []byte(`{"content":"round 1 started"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	j := job.Job{
		Name: "prjob", Brief: filepath.Join(dir, "brief.md"),
		Worktree: dir, SessionName: "prjob", SessionPath: sess,
		MaxRounds: 2, TimeoutS: 60, CIStallCap: 1, CIStallIdleS: 1,
	}
	if err := os.WriteFile(j.Brief, []byte("PR TEST"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	return s, sess
}

func appendTranscript(t *testing.T, sess string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range lines {
		if _, err := f.WriteString(ln + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
}

// endToEnd: the transcript tail (the existing stall detector) finds the PR,
// the supervisor records it on the runner, `status` carries it, and every
// terminal event payload carries it too (ADR-0006).
func TestPRURLFromTranscriptSurfacesInStatusAndEvents(t *testing.T) {
	s, sess := prFixture(t)
	r := s.jobs["prjob"]
	// Same code path the per-round watcher uses: the detector starts at the
	// file end, then the agent's link to the PR lands.
	d := stall.New(sess, time.Hour)
	appendTranscript(t, sess,
		`{"type":"message","role":"assistant","content":"Opened the PR: `+realPRURL+`"}`)
	d.Poll()
	s.recordPR(r, d.PRURL())

	// status <job> carries it (this is what the JSON the CLI prints contains).
	data, err := s.Status("prjob")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	if !strings.Contains(string(raw), `"pr_url":"`+realPRURL+`"`) {
		t.Fatalf("status payload missing pr_url: %s", raw)
	}
	var st job.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.PRURL != realPRURL {
		t.Fatalf("Status.PRURL = %q, want %q", st.PRURL, realPRURL)
	}

	// Persisted so a daemon restart / `status` after the fact still shows it.
	r.persistState()
	saved, err := job.LoadState("prjob")
	if err != nil {
		t.Fatal(err)
	}
	if saved.PRURL != realPRURL {
		t.Fatalf("persisted State.PRURL = %q, want %q", saved.PRURL, realPRURL)
	}

	// Every terminal event kind carries pr_url.
	for _, kind := range []string{"done", "fatal", "stopped", "instant_exit"} {
		s.emit("prjob", kind, 2, 0, 7, "", "terminal %s", kind)
	}
	audit, err := os.ReadFile(events.File("prjob"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(audit)), "\n") {
		var ev events.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.PRURL != realPRURL {
			t.Fatalf("event %s pr_url = %q, want %q", ev.Event, ev.PRURL, realPRURL)
		}
	}
}

// No PR linked in the transcript => pr_url stays absent everywhere. That is
// the documented best-effort behavior, not an error.
func TestPRURLAbsentWhenTranscriptHasNoURL(t *testing.T) {
	s, sess := prFixture(t)
	appendTranscript(t, sess,
		`{"content":"opened a PR but did not paste the link"}`,
		`{"content":"see https://github.com/Belphemur/XPoint/issues/184"}`,
	)
	r := s.jobs["prjob"]
	d := stall.New(sess, time.Hour)
	appendTranscript(t, sess, `{"content":"no pull request here either"}`)
	d.Poll()
	s.recordPR(r, d.PRURL())

	data, err := s.Status("prjob")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "pr_url") {
		t.Fatalf("status should omit pr_url when empty: %s", raw)
	}
	s.emit("prjob", "done", 1, 0, 3, "", "done")
	audit, _ := os.ReadFile(events.File("prjob"))
	if strings.Contains(string(audit), "pr_url") {
		t.Fatalf("event should omit pr_url when empty: %s", audit)
	}
}
