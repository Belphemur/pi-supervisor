package supervisor

import (
	"fmt"
	"sync"
	"time"
)

// reviewCampaign is the per-job state of a review loop (ADR-0012 §4).
//
// It is deliberately SEPARATE from job.State: a campaign has its own round
// counter and its own MaxRounds, and the build job's `done` verdict must
// survive into the final report. Merging the two would let a review round
// reset the build job's accounting (or vice versa).
type reviewCampaign struct {
	mu       sync.Mutex
	pr       int
	owner    string
	repo     string
	round    int
	maxRound int
	// spent counts rounds that actually DID something (assistant text on
	// the transcript): the budget is spent on work, not on provider flakes
	// that returned an empty 0-token response and produced nothing
	// (flambette#65 rounds 5-6). Exhaustion reads this, not the raw loop
	// round. Counted from the RESULT text, not the runlog — the runlog
	// also mirrors client diagnostics, so a forwarded steer's diag line
	// would count an empty turn as work (qodo PR#9 finding 3).
	kind  string // acceptance|rebuttal
	spent int
	// rawRound counts review rounds SINCE THE CAMPAIGN BEGAN — the
	// backstop's denominator. r.state.Round is the job's LIFETIME counter
	// (build rounds included) and is never reset on the build→reviewing
	// transition, so comparing it against maxRound*3 exhausted campaigns
	// whose build ran long before the first review round (qodo PR#9
	// finding 2 / kody). Bumped alongside spent, under the same lock.
	rawRound int
	// answered records, per round, which threads this job posted a reply on.
	// The resolve guard reads it. Keyed by round so a reply from round N-1
	// does NOT authorize a resolve in round N: the ADR requires the answer and
	// the close to belong to the same round.
	answered map[int]map[string]bool
	// pendingAcks holds bulk_resolve requests awaiting a peer ACK, with their
	// expiry. An unacked request never applies; it just expires (fail closed).
	pendingAcks map[string]*pendingAck
	ackSeq      int
	// lastOpen is the open-thread count from the most recent gate evaluation.
	// It exists so an EXHAUSTED campaign can record a truthful baseline: that
	// path ends with threads still open, so the baseline must be that count and
	// not 0, or the next re-check flags every one of them as brand new.
	lastOpen int
}

// setLastOpen records the open-thread count observed by a gate evaluation.
func (c *reviewCampaign) setLastOpen(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastOpen = n
}

// lastOpenCount returns the most recently observed open-thread count.
func (c *reviewCampaign) lastOpenCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastOpen
}

type pendingAck struct {
	ID        string
	Job       string
	PR        int
	ThreadIDs []string
	Reason    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// DefaultAckTimeout is how long an unacked bulk_resolve stays pending before
// it is abandoned. Fail-closed: threads simply remain open.
const DefaultAckTimeout = 30 * time.Minute

// newReviewCampaign builds a campaign for a PR.
func newReviewCampaign(owner, repo string, pr, maxRounds int, kind string) *reviewCampaign {
	if maxRounds <= 0 {
		maxRounds = 5
	}
	return &reviewCampaign{
		pr:          pr,
		owner:       owner,
		repo:        repo,
		maxRound:    maxRounds,
		kind:        kind,
		answered:    map[int]map[string]bool{},
		pendingAcks: map[string]*pendingAck{},
	}
}

// markAnswered records that this job posted a reply on threadID in round.
// Called only after the daemon actually applied the reply, so the guard
// cannot be satisfied by a request that failed.
func (c *reviewCampaign) markAnswered(round int, threadIDs ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	set, ok := c.answered[round]
	if !ok {
		set = map[string]bool{}
		c.answered[round] = set
	}
	for _, id := range threadIDs {
		set[id] = true
	}
}

// answeredInRound reports whether threadID received a reply in that exact
// round. Scoping to the round is the point: "some round answered it" would let
// a stale reply authorize a close long after the finding was superseded.
func (c *reviewCampaign) answeredInRound(round int, threadID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.answered[round][threadID]
}

// addPendingAck registers a bulk-resolve awaiting ACK and returns its id.
// The id is what `pi-supervisor ack --event <id>` takes.
func (c *reviewCampaign) addPendingAck(jobName string, ids []string, reason string, timeout time.Duration) *pendingAck {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ackSeq++
	now := time.Now()
	id := fmt.Sprintf("brq-%d-%d-%s", c.pr, c.round, shortID(now, c.ackSeq))
	pa := &pendingAck{
		ID:        id,
		Job:       jobName,
		PR:        c.pr,
		ThreadIDs: ids,
		Reason:    reason,
		CreatedAt: now,
		ExpiresAt: now.Add(timeout),
	}
	c.pendingAcks[id] = pa
	return pa
}

// takeAck removes and returns the pending ack for id, if present. Removal on
// read makes an ACK single-use: a second ack for the same id finds nothing.
func (c *reviewCampaign) takeAck(id string) (*pendingAck, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pa, ok := c.pendingAcks[id]
	if ok {
		delete(c.pendingAcks, id)
	}
	return pa, ok
}

// expireAcks drops pending requests past their deadline and returns them so
// the caller can emit bulk_resolve_expired. Fail-closed: an expired request
// applies NOTHING, leaving the threads open.
func (c *reviewCampaign) expireAcks(now time.Time) []*pendingAck {
	c.mu.Lock()
	defer c.mu.Unlock()
	var gone []*pendingAck
	for id, pa := range c.pendingAcks {
		if now.After(pa.ExpiresAt) {
			gone = append(gone, pa)
			delete(c.pendingAcks, id)
		}
	}
	return gone
}

// shortID makes a readable, collision-resistant-enough ack suffix from the
// clock plus a per-campaign counter. It is an opaque handle for an agent to
// echo back, not a security token: the socket is 0600 and the daemon is
// already bound to a live round.
func shortID(t time.Time, seq int) string {
	return fmt.Sprintf("%d%02d", t.Unix()%100000, seq)
}
