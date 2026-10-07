package supervisor

// Mid-round thread reminders (ADR-0019).
//
// The campaign gate (ADR-0012 §4.2) evaluates threads BETWEEN rounds; inside
// a round the daemon was blind. A round that drifts away from the open
// threads burned its whole budget only for the gate to rediscover — after
// the client exited — what this watcher could have said minutes earlier:
// which threads are still open and what their last comments ask.
//
// The watcher runs alongside the other round watchers (watchCIStalls,
// watchEmptyTurn), lists open threads on a ticker, and STEERS the live
// session with a digest of the open threads not yet answered this round. It
// never signals pi (a reminder is advisory; --interrupt stays an operator
// decision, ADR-0007) and never outlives the round it serves (push-only
// delivery, invariant 6).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"pi-supervisor/internal/review"
)

// Reminder cadence. Package vars so tests can shorten them; deliberately not
// job-def surface — the defaults are right for every campaign observed.
var (
	reviewRemindDelay  = 2 * time.Minute  // first reminder this deep into the round
	reviewRemindRepeat = 10 * time.Minute // re-remind an UNCHANGED open set this often
	reviewRemindTick   = 30 * time.Second // re-check cadence (change => sooner)
)

// snippetCap keeps the digest one steer even with many threads: the model
// that needs full history has thread_detail (ADR-0012 §3.3).
const snippetCap = 280

// watchThreadReminders serves ONE reviewing round. watchStop/stopCh follow
// the watcher protocol of watchCIStalls: any close ends the goroutine, and
// loop() waits for its exit before classifying the round.
func (s *Supervisor) watchThreadReminders(r *runner, round int, watchStop, stopCh chan struct{}) {
	r.mu.Lock()
	camp := r.review
	name := r.job.Name
	r.mu.Unlock()
	if camp == nil {
		return // not a campaign round; build jobs never see reminders
	}
	camp.mu.Lock()
	owner, repo, pr := camp.owner, camp.repo, camp.pr
	camp.mu.Unlock()

	started := time.Now()
	var lastSig string
	var lastSteer time.Time

	check := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		gh, err := gateReader(s, ctx, owner, repo)
		if err != nil {
			s.logf(name, "round %d: thread reminder skipped (no GitHub read: %v)", round, err)
			return
		}
		all, err := gh.ListThreads(ctx, owner, repo, pr)
		if err != nil {
			s.logf(name, "round %d: thread reminder skipped (list failed: %v)", round, err)
			return
		}
		// Open AND not answered in THIS round: an answered-but-unresolved
		// thread is already authorized to close (answer-before-resolve,
		// ADR-0012 §3.4) — reminding about it would push a double-answer.
		var pending []review.Thread
		for _, t := range review.OpenThreads(all) {
			if !camp.answeredInRound(round, t.ThreadID) {
				pending = append(pending, t)
			}
		}
		if len(pending) == 0 {
			return
		}
		sig := reminderSignature(pending)
		now := time.Now()
		unchanged := sig == lastSig
		if unchanged && now.Sub(lastSteer) < reviewRemindRepeat {
			return
		}
		if lastSig == "" && now.Sub(started) < reviewRemindDelay {
			return
		}

		text := reminderDigest(owner, repo, pr, round, pending)
		rep, err := s.Steer(name, text, true, false)
		if err != nil {
			// `no live round` is the normal outcome in the seconds before the
			// client exits (the watcher is stopped right after); journal it,
			// never spam an event for a race the gate already handles.
			s.logf(name, "round %d: thread reminder not delivered: %v", round, err)
			return
		}
		lastSig, lastSteer = sig, now
		s.logf(name, "round %d: thread reminder steered (%d open, outcome=%s)", round, len(pending), rep.Outcome)
		s.emit(name, "review_reminder", round, 0, 0, "",
			"%d open review thread(s) re-steered into round %d (outcome=%s)", len(pending), round, rep.Outcome)
	}

	t := time.NewTicker(reviewRemindTick)
	defer t.Stop()
	for {
		select {
		case <-watchStop:
			return
		case <-stopCh:
			return
		case <-t.C:
			check()
		}
	}
}

// reminderSignature identifies a set of open threads: sorted ids plus, per
// thread, the last comment's body — a NEW comment on the same thread is new
// information worth a reminder even though the id set did not grow.
func reminderSignature(ts []review.Thread) string {
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ThreadID+"|"+t.Body)
	}
	sort.Strings(ids)
	return strings.Join(ids, "\n")
}

// reminderDigest is the steer text: the exact input the shim's reply/resolve
// verbs consume, so the model can act without a round-trip.
func reminderDigest(owner, repo string, pr, round int, pending []review.Thread) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Supervisor review reminder (round %d): %d review thread(s) on %s/%s#%d are still open and have no answer from this round. Address each one (reply, then resolve) before finishing the round:\n",
		round, len(pending), owner, repo, pr)
	for _, t := range pending {
		body := strings.TrimSpace(t.Body)
		if len(body) > snippetCap {
			body = body[:snippetCap] + "…"
		}
		if body == "" {
			body = "(no comment body)"
		}
		fmt.Fprintf(&b, "- %s — last by %s: %s\n", t.ThreadID, t.LastAuthor, body)
	}
	return strings.TrimRight(b.String(), "\n")
}
