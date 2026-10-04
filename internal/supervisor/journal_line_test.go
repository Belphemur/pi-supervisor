package supervisor

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"pi-supervisor/internal/job"
	"pi-supervisor/internal/journal"
)

// journalSink redirects the journal package sink for the duration of a test.
type journalSink struct {
	b    bytes.Buffer
	prev io.Writer
}

func (j *journalSink) String() string { return j.b.String() }
func (j *journalSink) restore()       { journal.SetOutput(j.prev) }

// A fatal job is the one thing an operator must see in `systemctl status`, so
// it rides along on the watchdog's STATUS= line — the count alone
// ("0 parallel pi session(s) running") explains nothing.
func TestStatusLineNamesFatalJobs(t *testing.T) {
	s := New()
	s.jobs["zeta"] = &runner{job: job.Job{Name: "zeta"}, state: job.State{State: "fatal"}}
	s.jobs["alpha"] = &runner{job: job.Job{Name: "alpha"}, state: job.State{State: "fatal"}}
	s.jobs["busy"] = &runner{job: job.Job{Name: "busy"}, active: true, state: job.State{State: "running"}}

	n, note := s.StatusLine()
	if n != 1 || s.RunningCount() != 1 {
		t.Fatalf("RunningCount = %d/%d, want 1", n, s.RunningCount())
	}
	if note != "FATAL: alpha,zeta" {
		t.Fatalf("note = %q, want sorted fatal names", note)
	}

	s.jobs["zeta"].state.State = "done"
	if _, note := s.StatusLine(); note != "FATAL: alpha" {
		t.Fatalf("note = %q after clearing one fatal job", note)
	}
}

// Every lifecycle transition already flows through emit(), so the journal
// line is emitted there: one call site means the log cannot drift from the
// event stream.
func TestEmitJournalsTransitions(t *testing.T) {
	var sink journalSink
	sink.prev = journal.SetOutput(&sink.b)
	defer sink.restore()
	s := New()

	s.emit("j", "fatal", 2, 1, 30, "", "round cap %d reached", 6)
	line := strings.TrimSpace(sink.String())
	for _, want := range []string{
		"ERROR job event=job_fatal", "job=j", "round=2", "rc=1",
		"dur_s=30", `detail="round cap 6 reached"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q lacks %q", line, want)
		}
	}
}
