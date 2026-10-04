package supervisor

import (
	"errors"
	"path/filepath"
	"testing"

	"pi-supervisor/internal/job"
	"pi-supervisor/internal/review"
)

// mkJob builds a minimal loadable job definition for review-surface tests.
func mkJob(t *testing.T, name string) job.Job {
	t.Helper()
	return job.Job{
		Name: name, Brief: filepath.Join(t.TempDir(), "b.md"),
		Worktree: t.TempDir(), SessionName: name,
		MaxRounds: 1, TimeoutS: 5, PiBin: "true",
	}
}

// Authorization is checked BEFORE any input validation or GitHub call. That
// ordering is deliberate: a request from outside a live round must learn
// nothing at all — not whether its thread id was well-formed, not whether the
// PR exists. Validating first would leak that information to any caller who
// can reach the socket.
func TestReviewActionRefusesWithoutLiveRound(t *testing.T) {
	testEnv(t)
	writeJob(t, mkJob(t, "idle"))
	s := newTestSupervisor(t)

	cases := []struct {
		name string
		act  reviewAction
	}{
		{"valid-looking resolve", reviewAction{
			Action: "resolve_thread", Job: "idle",
			ThreadID: "PRRT_kwDOUDrzps6cpWH0"}},
		{"malformed comment id", reviewAction{
			Action: "resolve_thread", Job: "idle", ThreadID: "3904873498"}},
		{"list_threads", reviewAction{Action: "list_threads", Job: "idle", PR: 43}},
		{"unknown verb", reviewAction{Action: "no_such_verb", Job: "idle"}},
		{"bulk_resolve", reviewAction{
			Action: "bulk_resolve", Job: "idle", PR: 43,
			ThreadIDs: []string{"PRRT_a"}, Reason: "out of scope"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := s.ReviewAction(t.Context(), c.act)
			if res.OK {
				t.Fatal("a request with no live review round must be refused")
			}
			if res.Reason != string(review.ReasonNoLiveRound) {
				t.Fatalf("reason = %q, want %q (validation must not run first)",
					res.Reason, review.ReasonNoLiveRound)
			}
			if res.Refusal != nil && res.Refusal.ExitCode() != 2 {
				t.Errorf("exit class = %d, want 2", res.Refusal.ExitCode())
			}
		})
	}
}

// An unknown job is refused by name, never silently treated as "no round".
func TestReviewActionUnknownJob(t *testing.T) {
	testEnv(t)
	s := newTestSupervisor(t)
	res := s.ReviewAction(t.Context(), reviewAction{Action: "list_threads", Job: "ghost"})
	if res.OK {
		t.Fatal("an unknown job must be refused")
	}
	if res.Reason != string(review.ReasonUsage) {
		t.Fatalf("reason = %q, want usage", res.Reason)
	}
}

// A campaign with no pending ack must not let an ack through: the ack verb is
// the only path that closes many threads, so an invented id applies nothing.
func TestAckRejectsUnknownAckID(t *testing.T) {
	testEnv(t)
	writeJob(t, mkJob(t, "ackjob"))
	s := newTestSupervisor(t)

	// No campaign is live, so the ack is refused before the id is even looked
	// up — same fail-closed ordering as the action surface.
	_, err := s.AckBulkResolve(t.Context(), "ackjob", "brq-made-up")
	if err == nil {
		t.Fatal("acking with no live campaign must fail")
	}
	var r *review.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("error is not a Refusal: %v", err)
	}
	if r.Reason != review.ReasonNoLiveRound {
		t.Fatalf("reason = %q, want %q", r.Reason, review.ReasonNoLiveRound)
	}
}

// Arming the one-shot auto-review stanza must not itself start a campaign,
// and the stanza must be consumed exactly once.
func TestArmAutoReviewIsOneShot(t *testing.T) {
	testEnv(t)
	writeJob(t, mkJob(t, "autojob"))
	s := newTestSupervisor(t)

	if _, err := s.ArmAutoReview("autojob", 3, "rebuttal"); err != nil {
		t.Fatalf("ArmAutoReview: %v", err)
	}
	s.mu.Lock()
	r := s.jobs["autojob"]
	s.mu.Unlock()
	r.mu.Lock()
	spec := r.autoReview
	r.mu.Unlock()
	if spec == nil || spec.rounds != 3 || spec.kind != "rebuttal" {
		t.Fatalf("stanza = %+v, want rounds=3 kind=rebuttal", spec)
	}

	// Firing consumes it: the marker latch is sticky, so leaving it armed
	// would re-arm on every subsequent round and loop forever.
	handoff := s.autoReviewHandoff("autojob", r)
	if handoff {
		t.Fatal("handoff must not fire with no linked PR")
	}
	r.mu.Lock()
	still := r.autoReview
	r.mu.Unlock()
	if still != nil {
		t.Error("the stanza must be consumed on fire, even when the trigger cannot proceed")
	}
}
