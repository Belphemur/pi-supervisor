package supervisor

// Mid-round thread reminders (ADR-0019) — tests drive the REAL loop with the
// fake pi: a reviewing round steered with the open-thread digest while it is
// live, periodic re-reminders on an unchanged set, and the answer-before-
// resolve exclusion (threads answered this round are NOT re-reminded).

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/events"
	"pi-supervisor/internal/job"
	"pi-supervisor/internal/review"
)

// reminderJob builds a done job whose campaign brief makes fake-pi hold the
// round open long enough for the watcher to fire (SECS=8).
func reminderJob(t *testing.T, name string) (job.Job, string) {
	t.Helper()
	j, buildSess := doneJobWithReview(t, name, "yes")
	if err := os.WriteFile(j.ReviewBrief, []byte("TEST_SLOW SECS=8"), 0o644); err != nil {
		t.Fatal(err)
	}
	return j, buildSess
}

// reminderCadence shortens the watcher's clocks for the test.
func reminderCadence(t *testing.T) {
	t.Helper()
	delay, repeat, tick := reviewRemindDelay, reviewRemindRepeat, reviewRemindTick
	reviewRemindDelay, reviewRemindRepeat, reviewRemindTick = 300*time.Millisecond, 2*time.Second, 250*time.Millisecond
	t.Cleanup(func() {
		reviewRemindDelay, reviewRemindRepeat, reviewRemindTick = delay, repeat, tick
	})
}

// waitReminderEvent reads the watch stream until the nth review_reminder
// arrives (or the deadline dies).
func waitReminderEvent(t *testing.T, ch <-chan events.Event, n int) events.Event {
	t.Helper()
	deadline := time.After(30 * time.Second)
	seen := 0
	for {
		select {
		case e := <-ch:
			if e.Event == "review_reminder" {
				seen++
				if seen == n {
					return e
				}
			}
		case <-deadline:
			t.Fatalf("only %d/%d review_reminder event(s) arrived", seen, n)
		}
	}
}

// assertNoReminder pins the absence of a further reminder for a short window.
func assertNoReminder(t *testing.T, ch <-chan events.Event, window time.Duration) {
	t.Helper()
	select {
	case e := <-ch:
		if e.Event == "review_reminder" {
			t.Fatalf("unexpected review_reminder: round %d", e.Round)
		}
		// other events (round_done, backoff...) are fine
		assertNoReminder(t, ch, window)
	case <-time.After(window):
	}
}

// mustRead reads a small file, failing the test on error.
func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// waitAcked polls the ack log until SOME frame has a terminal ack record
// (forwarded/held→terminal/written). The reminder frame's id is daemon-
// generated, so any terminal record after the steer proves the plumbing.
func waitAcked(t *testing.T, ackPath string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		recs := readAcks(t, ackPath)
		for _, a := range recs {
			if a.Outcome == job.AckForwarded || a.Outcome == job.AckWritten ||
				(a.Outcome != job.AckHeld && a.Outcome != "") {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no terminal ack record in %s", ackPath)
}

// readAcks parses the ack JSONL, tolerating a missing file (empty result).
func readAcks(t *testing.T, path string) []job.AckRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []job.AckRecord
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var a job.AckRecord
		if err := json.Unmarshal([]byte(line), &a); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// A reviewing round gets steered with the digest of open threads while it is
// still running, and an UNCHANGED open set is re-reminded after the repeat
// window.
func TestThreadReminderSteersLiveSession(t *testing.T) {
	testEnv(t)
	reminderCadence(t)
	j, _ := reminderJob(t, "remind1")

	installGateReader(t, &fakeGateReader{
		threads: []review.Thread{
			{ThreadID: "PRRT_r1", LastAuthor: "coderabbit[bot]", Body: "unused variable in foo.go:12"},
			{ThreadID: "PRRT_r2", LastAuthor: "coderabbit[bot]", Body: "missing test for vacuum"},
		},
		roll: &review.CIRollup{Verdict: "pass"},
	})

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	doneShape(t, s, j.Name)

	if _, err := s.StartReview(context.Background(), j.Name, 54, 1, "acceptance"); err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	// Subscribe AFTER StartReview: Watch on a DONE job answers with a precheck
	// and closes the stream (invariant 6) — the reminder events would land on
	// a dead channel. From a live job the stream is open.
	ch, cancel, _ := s.Watch(j.Name)
	defer cancel()

	first := waitReminderEvent(t, ch, 1)
	if first.Round < 1 {
		t.Fatalf("reminder event carries round %d", first.Round)
	}
	// The digest must be IN the ctrl file the live client reads, and the
	// client must have acked the frame (outcome recorded in the ack log).
	ctrl := mustRead(t, job.Ctrl(j.Name))
	if !strings.Contains(ctrl, "Supervisor review reminder") || !strings.Contains(ctrl, "PRRT_r1") || !strings.Contains(ctrl, "PRRT_r2") {
		t.Fatalf("ctrl file lacks the reminder digest:\n%s", ctrl)
	}
	waitAcked(t, job.Ack(j.Name))

	second := waitReminderEvent(t, ch, 2) // unchanged set, repeat window
	if second.Round != first.Round {
		t.Logf("note: re-reminder landed in round %d (first was %d)", second.Round, first.Round)
	}

	_ = s.Stop(j.Name)
}

// Threads answered in the CURRENT round are excluded: marking one answered
// stops reminders about it even though it is still unresolved on GitHub.
func TestThreadReminderSkipsAnsweredInRound(t *testing.T) {
	testEnv(t)
	reminderCadence(t)
	j, _ := reminderJob(t, "remind2")

	installGateReader(t, &fakeGateReader{
		threads: []review.Thread{
			{ThreadID: "PRRT_a1", LastAuthor: "coderabbit[bot]", Body: "rename this"},
		},
		roll: &review.CIRollup{Verdict: "pass"},
	})

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	doneShape(t, s, j.Name)

	if _, err := s.StartReview(context.Background(), j.Name, 54, 1, "acceptance"); err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	// Same subscribe-after-Start rule as the steer test above.
	ch, cancel, _ := s.Watch(j.Name)
	defer cancel()

	first := waitReminderEvent(t, ch, 1)
	s.mu.Lock()
	r := s.jobs[j.Name]
	r.mu.Lock()
	camp := r.review
	r.mu.Unlock()
	s.mu.Unlock()
	if camp == nil {
		t.Fatal("campaign gone before markAnswered")
	}
	camp.markAnswered(first.Round, "PRRT_a1")

	// With the only open thread answered this round, no further reminder may
	// fire (repeat window far out; several ticks pass meanwhile).
	assertNoReminder(t, ch, 3*time.Second)

	_ = s.Stop(j.Name)
}

// Build rounds (no campaign armed) never see reminders — the watcher must be
// a no-op there, not an error source.
func TestThreadReminderNoopWithoutCampaign(t *testing.T) {
	testEnv(t)
	reminderCadence(t)
	stop := make(chan struct{})
	done := make(chan struct{})
	r := &runner{job: job.Job{Name: "nocamp"}}
	go func() {
		defer close(done)
		(New()).watchThreadReminders(r, 1, stop, make(chan struct{}))
	}()
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exit after watchStop")
	}
}
