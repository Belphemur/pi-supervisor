package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// withThreadCount stubs the GitHub read for the duration of one test.
func withThreadCount(t *testing.T, n int) {
	t.Helper()
	prev := listOpenThreads
	listOpenThreads = func(*Supervisor, context.Context, string, string, int) (int, error) {
		return n, nil
	}
	t.Cleanup(func() { listOpenThreads = prev })
}

func alertJob(t *testing.T, name string, openAtClose int) (*Supervisor, *runner) {
	t.Helper()
	testEnv(t)
	pi := fakePiPath(t)
	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: name, Brief: brief, Worktree: dir, SessionName: name,
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)
	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs[name]
	s.recordThreadBaseline(name, r, "Belphemur", "pi-supervisor", 3, openAtClose)
	r.mu.Lock()
	r.active = false // campaign ended
	r.mu.Unlock()
	return s, r
}

// TestAlertFiresWhenThreadsAppeared is the ALERT path — previously untestable,
// because the only route to a thread count was a live GitHub client, so every
// assertion stopped at "the code tried to dial GitHub". That is how the empty
// owner shipped with a green suite: the decision it guards was never exercised.
//
// This is the scenario from PR #3: campaign closed at 0 open threads, the PR
// later gained findings, and nothing announced them.
func TestAlertFiresWhenThreadsAppeared(t *testing.T) {
	withThreadCount(t, 2) // two new findings arrived
	s, _ := alertJob(t, "alert", 0)

	ch, cancel, _ := s.Watch("alert")
	defer cancel()

	res, err := s.RecheckThreads(t.Context(), "alert")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type %T", res)
	}
	if intOf(m["new_threads"]) != 2 {
		t.Fatalf("new_threads = %v, want 2 (0 at close, 2 now)", m["new_threads"])
	}
	if intOf(m["open_now"]) != 2 || intOf(m["open_at_close"]) != 0 {
		t.Fatalf("counts wrong: %+v", m)
	}
	action := strOf(m["action"])
	if action == "" || !strings.Contains(action, "--pr 3") {
		t.Fatalf("action must name the re-arm command with the PR: %q", action)
	}

	// And the event must reach a subscriber.
	select {
	case ev := <-ch:
		if ev.Event != "review_threads_appeared" {
			t.Fatalf("got %q, want review_threads_appeared", ev.Event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no review_threads_appeared event was delivered")
	}

	// State must reflect the new counts.
	st, err := job.LoadState("alert")
	if err != nil {
		t.Fatal(err)
	}
	if st.ReviewBaseline.NewSinceClose != 2 || st.ReviewBaseline.OpenNow != 2 {
		t.Fatalf("persisted counts wrong: %+v", st.ReviewBaseline)
	}
}

// The mirror case: fewer threads than the baseline must NOT alert. Without this,
// a re-check that reported "new threads" on every push would be trusted until it
// cried wolf.
func TestNoAlertWhenThreadsShrinkOrHold(t *testing.T) {
	for _, tc := range []struct {
		name         string
		atClose, now int
	}{
		{"unchanged", 3, 3},
		{"fewer", 5, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withThreadCount(t, tc.now)
			s, _ := alertJob(t, "noalert-"+tc.name, tc.atClose)
			ch, cancel, _ := s.Watch("noalert-" + tc.name)
			defer cancel()

			res, err := s.RecheckThreads(t.Context(), "noalert-"+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			m, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("unexpected type %T", res)
			}
			if intOf(m["new_threads"]) != 0 {
				t.Fatalf("new_threads = %v, want 0", m["new_threads"])
			}
			if strings.Contains(strOf(m["action"]), "re-arm") {
				t.Fatalf("action should not ask for a re-arm: %q", strOf(m["action"]))
			}
			select {
			case ev := <-ch:
				t.Fatalf("unexpected event %q", ev.Event)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// An EXHAUSTED campaign leaves threads open, so its baseline is non-zero. A
// later re-check must measure against THAT, not against zero, or every
// still-open thread reads as brand new.
func TestExhaustedBaselineMeasuresAgainstRealCount(t *testing.T) {
	withThreadCount(t, 7) // nothing new since the exhausted close
	s, _ := alertJob(t, "exhausted", 7)

	res, err := s.RecheckThreads(t.Context(), "exhausted")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type %T", res)
	}
	if intOf(m["new_threads"]) != 0 {
		t.Fatalf("7 open against a baseline of 7 is NOT new activity: %+v", m)
	}
}
