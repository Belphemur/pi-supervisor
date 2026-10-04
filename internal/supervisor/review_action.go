package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"pi-supervisor/internal/review"
)

// reviewAction is the /review/action request body (ADR-0012 §3).
//
// `round` and `job` are accepted in the payload but IGNORED: the daemon stamps
// both from its own live runner (ADR-0012 §8, Q6). If the shim could name its
// own round the answer-before-resolve guard would be self-certifying — the
// exact false-delivery class ADR-0005 exists to kill. A mismatch is refused,
// never silently corrected.
type reviewAction struct {
	Action    string          `json:"action"`
	Job       string          `json:"job,omitempty"`
	PR        int             `json:"pr,omitempty"`
	ThreadID  string          `json:"thread_id,omitempty"`
	ThreadIDs []string        `json:"thread_ids,omitempty"`
	Round     int             `json:"round,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	State     string          `json:"state,omitempty"`
	Replies   []reviewReply   `json:"replies,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

// reviewReply is one (thread_id, type, body) tuple in a post_replies batch.
type reviewReply struct {
	ThreadID string `json:"thread_id"`
	Type     string `json:"type"` // acceptance|rebuttal
	Body     string `json:"body"`
}

// reviewResult is the /review/action response payload.
type reviewResult struct {
	OK      bool            `json:"ok"`
	Reason  string          `json:"reason,omitempty"`
	Message string          `json:"message,omitempty"`
	Job     string          `json:"job,omitempty"`
	Round   int             `json:"round,omitempty"`
	PR      int             `json:"pr,omitempty"`
	Payload any             `json:"payload,omitempty"`
	Refusal *review.Refusal `json:"-"`
	_       struct{}        // keep the struct non-comparable-safe for future fields
}

// refuseResult renders a refusal so it reaches the CLI as data, not prose.
func refuseResult(r *review.Refusal, jobName string, round int) reviewResult {
	return reviewResult{
		OK:      false,
		Reason:  string(r.Reason),
		Message: r.Message,
		Job:     jobName,
		Round:   round,
		Refusal: r,
	}
}

// ReviewAction executes one review verb on behalf of the shim running inside
// a live pi round (ADR-0012 §2).
//
// The authorization model is structural, not credentialed: the shim runs
// inside pi, the daemon knows which job owns the live round, and the socket is
// 0600. A request when no review round is live is refused.
func (s *Supervisor) ReviewAction(ctx context.Context, act reviewAction) reviewResult {
	s.mu.Lock()
	r, ok := s.jobs[act.Job]
	s.mu.Unlock()
	if !ok {
		return refuseResult(review.ErrUnknownJob(act.Job), act.Job, 0)
	}
	camp, liveRound := s.liveReview(r)
	if camp == nil {
		return refuseResult(review.ErrNoLiveRound(act.Job), act.Job, 0)
	}
	// Round stamping: the client's claim is only ever CHECKED, never used.
	if act.Round != 0 && act.Round != liveRound {
		return refuseResult(
			review.ErrRoundMismatch(act.Round, liveRound), act.Job, liveRound)
	}
	gh, err := s.reviewClient(ctx, camp.owner, camp.repo)
	if err != nil {
		return refuseResult(asRefusal(err), act.Job, liveRound)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	res := reviewResult{OK: true, Job: act.Job, Round: liveRound, PR: camp.pr}
	switch act.Action {
	case "list_threads":
		payload, err := s.reviewListThreads(ctx, gh, camp, act)
		if err != nil {
			return refuseResult(asRefusal(err), act.Job, liveRound)
		}
		res.Payload = payload
	case "thread_detail":
		if act.ThreadID == "" {
			return refuseResult(review.ErrUsage("thread_detail requires thread_id"), act.Job, liveRound)
		}
		d, err := gh.ThreadDetail(ctx, camp.owner, camp.repo, act.ThreadID)
		if err != nil {
			return refuseResult(asRefusal(err), act.Job, liveRound)
		}
		res.Payload = d
	case "post_replies":
		payload, err := s.reviewPostReplies(ctx, gh, camp, act, liveRound)
		if err != nil {
			return refuseResult(asRefusal(err), act.Job, liveRound)
		}
		res.Payload = payload
	case "resolve_thread":
		if err := s.reviewResolve(ctx, gh, camp, act, liveRound); err != nil {
			return refuseResult(asRefusal(err), act.Job, liveRound)
		}
		res.Message = "resolved " + act.ThreadID
	case "bulk_resolve":
		payload, err := s.reviewBulkResolve(ctx, gh, camp, act, liveRound)
		if err != nil {
			return refuseResult(asRefusal(err), act.Job, liveRound)
		}
		res.Payload = payload
	case "check_ci":
		roll, err := gh.CheckCI(ctx, camp.owner, camp.repo, camp.pr)
		if err != nil {
			return refuseResult(asRefusal(err), act.Job, liveRound)
		}
		res.Payload = roll
	default:
		return refuseResult(review.ErrUsage("unknown action %q", act.Action), act.Job, liveRound)
	}
	return res
}

// reviewListThreads answers list_threads: the open-thread set plus the
// head sha and CI verdict the outer loop needs to decide the next round.
func (s *Supervisor) reviewListThreads(ctx context.Context, gh *review.Client, camp *reviewCampaign, act reviewAction) (any, error) {
	all, err := gh.ListThreads(ctx, camp.owner, camp.repo, camp.pr)
	if err != nil {
		return nil, err
	}
	open := review.OpenThreads(all)
	out := map[string]any{
		"threads": open,
		"total":   len(all),
		"open":    len(open),
		"cursor":  nil, // nil == pagination exhausted (never "page one")
	}
	if head, err := gh.PRHeadSHA(ctx, camp.owner, camp.repo, camp.pr); err == nil {
		out["head_sha"] = head
	}
	if roll, err := gh.CheckCI(ctx, camp.owner, camp.repo, camp.pr); err == nil {
		out["ci"] = roll.Verdict
	}
	return out, nil
}

// reviewPostReplies applies a batch of replies and records them against the
// CURRENT round, which is what later authorizes resolve_thread.
func (s *Supervisor) reviewPostReplies(ctx context.Context, gh *review.Client, camp *reviewCampaign, act reviewAction, round int) (any, error) {
	if len(act.Replies) == 0 {
		return nil, review.ErrUsage("post_replies requires a non-empty replies array")
	}
	applied := make([]string, 0, len(act.Replies))
	for _, rep := range act.Replies {
		if !review.ValidThreadID(rep.ThreadID) {
			return nil, review.ErrUnknownThread(rep.ThreadID)
		}
		if strings.TrimSpace(rep.Body) == "" {
			return nil, review.ErrUsage("reply for %s has an empty body", rep.ThreadID)
		}
		if err := gh.PostReply(ctx, rep.ThreadID, rep.Body); err != nil {
			return nil, err
		}
		applied = append(applied, rep.ThreadID)
	}
	// Recorded only after every reply actually landed, so a failed batch
	// never authorizes a close.
	camp.markAnswered(round, applied...)
	out := map[string]any{"applied": applied}
	if head, err := gh.PRHeadSHA(ctx, camp.owner, camp.repo, camp.pr); err == nil {
		out["head_sha"] = head
	}
	return out, nil
}

// reviewResolve enforces answer-before-resolve for ONE thread.
func (s *Supervisor) reviewResolve(ctx context.Context, gh *review.Client, camp *reviewCampaign, act reviewAction, round int) error {
	if !review.ValidThreadID(act.ThreadID) {
		return review.ErrUnknownThread(act.ThreadID)
	}
	if !camp.answeredInRound(round, act.ThreadID) {
		return review.ErrNotAnswered(act.ThreadID, round)
	}
	return gh.ResolveThread(ctx, act.ThreadID)
}

// reviewBulkResolve posts the audit comment, then registers a PENDING ack.
// It applies NOTHING yet: the mutations fire only after a peer ACK, and an
// unacked request expires to "threads stay open" (ADR-0012 §2.3).
func (s *Supervisor) reviewBulkResolve(ctx context.Context, gh *review.Client, camp *reviewCampaign, act reviewAction, round int) (any, error) {
	if len(act.ThreadIDs) == 0 {
		return nil, review.ErrUsage("bulk_resolve requires thread_ids")
	}
	for _, id := range act.ThreadIDs {
		if !review.ValidThreadID(id) {
			return nil, review.ErrUnknownThread(id)
		}
	}
	reason := strings.TrimSpace(act.Reason)
	if reason == "" {
		return nil, review.ErrUsage("bulk_resolve requires a reason (it is posted to the PR as the audit trail)")
	}
	audit := fmt.Sprintf("**pi-supervisor bulk resolve requested**\n\nReason: %s\n\nThreads:\n%s\n\n"+
		"This was requested by the review agent. It applies only after an explicit ack "+
		"(`pi-supervisor ack --event <ack_id>`); unacked requests expire and the threads stay open.",
		reason, bulletList(act.ThreadIDs))
	if err := gh.PostComment(ctx, camp.owner, camp.repo, camp.pr, audit); err != nil {
		return nil, err
	}
	pa := camp.addPendingAck(act.Job, act.ThreadIDs, reason, s.reviewAckTimeout())
	s.emit(act.Job, "bulk_resolve_requested", round, 0, 0, "", "bulk resolve requested for %d thread(s), ack %s", len(act.ThreadIDs), pa.ID)
	return map[string]any{
		"pending_ack": true,
		"ack_id":      pa.ID,
		"threads":     act.ThreadIDs,
		"expires_at":  pa.ExpiresAt.UTC().Format(time.RFC3339),
	}, nil
}

// reviewAckTimeout reads the ack deadline, with the fail-safe default.
func (s *Supervisor) reviewAckTimeout() time.Duration {
	s.mu.Lock()
	d := s.reviewAckTimeoutDur
	s.mu.Unlock()
	if d <= 0 {
		return DefaultAckTimeout
	}
	return d
}

func bulletList(ids []string) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString("- `" + id + "`\n")
	}
	return b.String()
}

// AckBulkResolve applies a previously requested bulk resolve, once a peer has
// acked it. This is the ONLY path that closes many threads at once, and it is
// deliberately a separate, explicit verb: nothing closes silently.
func (s *Supervisor) AckBulkResolve(ctx context.Context, jobName, ackID string) (any, error) {
	s.mu.Lock()
	r, ok := s.jobs[jobName]
	s.mu.Unlock()
	if !ok {
		return nil, review.ErrUnknownJob(jobName)
	}
	camp, _ := s.liveReview(r)
	if camp == nil {
		return nil, review.ErrNoLiveRound(jobName)
	}
	pa, ok := camp.takeAck(ackID)
	if !ok {
		return nil, review.ErrUsage("no pending bulk_resolve with ack id %q (already acked, expired, or never issued)", ackID)
	}
	if time.Now().After(pa.ExpiresAt) {
		s.emit(jobName, "bulk_resolve_expired", 0, 0, 0, "", "ack %s arrived after expiry; nothing applied", ackID)
		return nil, review.ErrUsage("ack %s expired at %s; no threads were closed",
			ackID, pa.ExpiresAt.UTC().Format(time.RFC3339))
	}
	gh, err := s.reviewClient(ctx, camp.owner, camp.repo)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	failed := gh.BulkResolve(ctx, pa.ThreadIDs)
	s.emit(jobName, "bulk_resolve_applied", 0, 0, 0, "",
		"bulk resolve ack %s applied: %d closed, %d failed", ackID, len(pa.ThreadIDs)-len(failed), len(failed))
	return map[string]any{
		"acked":  ackID,
		"closed": len(pa.ThreadIDs) - len(failed),
		"failed": failed,
	}, nil
}

// ExpireReviewAcks drops pending bulk-resolve requests past their deadline.
// Called from the round loop so an unattended campaign cannot wedge on a gate
// nobody will visit: expiry is the fail-closed exit.
func (s *Supervisor) ExpireReviewAcks() {
	s.mu.Lock()
	runners := make([]*runner, 0, len(s.jobs))
	for _, r := range s.jobs {
		runners = append(runners, r)
	}
	s.mu.Unlock()
	now := time.Now()
	for _, r := range runners {
		r.mu.Lock()
		camp := r.review
		name := r.job.Name
		r.mu.Unlock()
		if camp == nil {
			continue
		}
		for _, pa := range camp.expireAcks(now) {
			s.emit(name, "bulk_resolve_expired", 0, 0, 0, "",
				"bulk resolve %s expired unacked after %s; %d thread(s) left open",
				pa.ID, DefaultAckTimeout, len(pa.ThreadIDs))
		}
	}
}

// liveReview returns the job's campaign and the round currently executing.
// The round comes from the LIVE runner, which is what makes the resolve guard
// un-forgeable by the client.
func (s *Supervisor) liveReview(r *runner) (*reviewCampaign, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.review == nil || !r.active || r.state.State != "running" {
		return nil, 0
	}
	return r.review, r.state.Round
}

// asRefusal narrows any error to the review Refusal shape so the CLI always
// sees a reason from the closed enum.
func asRefusal(err error) *review.Refusal {
	if err == nil {
		return nil
	}
	if r, ok := errors.AsType[*review.Refusal](err); ok {
		return r
	}
	return &review.Refusal{Reason: review.ReasonGitHubError, Message: err.Error()}
}
