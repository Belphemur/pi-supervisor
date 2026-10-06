package supervisor

// taskWatcher turns the client's TaskUpdate completion observations (ADR-0014)
// into published events through the ONE emit funnel. It owns the round-scoped
// pending state: correlation lives in the client (toolCallId execution
// identity); THIS layer confirms against the plugin's JSON and stamps job/round.
//
// Lifetimes are bounded by construction: one goroutine per round, channels are
// the only handoff (non-blocking sends in the client), and the worker drains
// whatever is buffered and exits when the round's client returns. Lookups run
// WITHOUT holding any supervisor lock: only publish takes s.mu/r.mu, briefly.

import (
	"path/filepath"
	"strings"
	"time"

	"pi-supervisor/internal/client"
	"pi-supervisor/internal/events"
	"pi-supervisor/internal/fault"
	"pi-supervisor/internal/taskwatch"
)

// flushBudget bounds the wait for in-flight observations to land before the
// round's terminal classification closes the watch. Generous, because the
// lookup is an open-read of a local file; but finite, so a wedged consumer can
// never hold the round loop.
const taskWatchFlushBudget = 10 * time.Second

// maxObservationsPerRound bounds one round's queue: an agent bulk-completing a
// long list still gets every observation processed, but a runaway loop is
// surfaced instead of misattributed or unbounded.
const maxObservationsPerRound = 512

// taskWatcher is the per-round consumer.
type taskWatcher struct {
	s        *Supervisor
	job      string
	round    int
	sess     string // pinned session path (for the fallback ID derivation)
	count    int
	overload bool
	ident    client.Identity
	hasID    bool
	identCh  chan client.Identity
	obsCh    chan client.Observation
	done     chan struct{} // closed by the round once the client returned
	exited   chan struct{} // closed by the worker when it drained
}

func newTaskWatcher(s *Supervisor, jobName, sess string, round int) *taskWatcher {
	return &taskWatcher{
		s:       s,
		job:     jobName,
		round:   round,
		sess:    sess,
		identCh: make(chan client.Identity, 1),
		obsCh:   make(chan client.Observation, 8),
		done:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
}

// run is the worker loop. It processes observations until the round's client
// returned AND the queue is drained (flush), bounded by obsCh's buffer.
func (tw *taskWatcher) run() {
	defer close(tw.exited)
	for {
		select {
		case id := <-tw.identCh:
			tw.ident, tw.hasID = id, true
		case ob := <-tw.obsCh:
			if tw.admit() {
				tw.process(ob)
			}
		case <-tw.done:
			// Flush: the client already closed pi's stream, so every
			// accepted observation is in the buffer. Drain non-blocking
			// until empty (bounded by the buffer itself), keeping the
			// identity pump alive alongside.
			for {
				select {
				case id := <-tw.identCh:
					tw.ident, tw.hasID = id, true
					continue
				case ob := <-tw.obsCh:
					if tw.admit() {
						tw.process(ob)
					}
					continue
				default:
					return
				}
			}
		}
	}
}

// admit bounds one round's queue: beyond maxObservationsPerRound the surplus
// is diagnosed, not silently misattributed (ADR-0014 §5).
func (tw *taskWatcher) admit() bool {
	tw.count++
	if tw.count <= maxObservationsPerRound || tw.overload {
		return tw.count <= maxObservationsPerRound
	}
	tw.overload = true
	tw.s.logf(tw.job, "WARN taskwatch: round %d exceeded %d eligible completions; "+
		"surplus is dropped and diagnosed", tw.round, maxObservationsPerRound)
	return false
}

// finish signals flush-and-exit and waits (bounded) for the drain.
func (tw *taskWatcher) finish() {
	close(tw.done)
	select {
	case <-tw.exited:
	case <-time.After(taskWatchFlushBudget):
		// The worker is wedged somewhere we cannot control (e.g. a blocked
		// consumer inside a suspicious emit). Leave it: having consumed the
		// budget is diagnosed at the call site.*/

	}
}

// process performs the confirmation lookup and publishes ONE event.
func (tw *taskWatcher) process(ob client.Observation) {
	// Identity before any I/O: ADR-0014 forbids a broad search across stores.
	id, ok := tw.resolveIdentity()
	if !ok {
		tw.publishFailure(ob, fault.KindTaskNoIdentity)
		return
	}
	res := taskwatch.Resolver{SessionID: id.SessionID, Cwd: id.Cwd}
	tgt := res.Resolve()
	if tgt.Memory {
		tw.publishFailure(ob, fault.KindTaskStoreMemory)
		return
	}
	if tgt.Unavailable {
		tw.publishFailure(ob, fault.KindTaskStoreMissing)
		return
	}
	info, confirmed, why := taskwatch.Lookup(tgt.Path, ob.TaskID)
	if !confirmed {
		tw.publishFailure(ob, taskwatchReason(why))
		return
	}
	tw.publish(ob, tgt.Path, info)
}

// resolveIdentity establishes (sessionID, cwd) from the running session. The
// RPC get_state reply is authoritative; the pinned session file name is the
// trusted fallback (pi names its transcripts <timestamp>_<sessionID>.jsonl).
// Cwd is the child's working directory = the job's worktree.
func (tw *taskWatcher) resolveIdentity() (client.Identity, bool) {
	if tw.hasID {
		return tw.ident, true
	}
	if tw.sess == "" {
		return client.Identity{}, false
	}
	sid := sessionIDFromPath(tw.sess)
	if sid == "" {
		return client.Identity{}, false
	}
	return client.Identity{SessionID: sid}, true
}

// sessionIDFromPath extracts the session ID from a pi transcript path:
// .../sessions/<dir>/<timestamp>_<uuid>.jsonl → <uuid>. Best-effort.
func sessionIDFromPath(path string) string {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".jsonl") {
		return ""
	}
	stem := strings.TrimSuffix(base, ".jsonl")
	i := strings.LastIndexByte(stem, '_')
	if i < 0 || i == len(stem)-1 {
		return ""
	}
	return stem[i+1:]
}

// publish forwards one confirmed completion through the funnel.
func (tw *taskWatcher) publish(ob client.Observation, taskFile string, info taskwatch.TaskInfo) {
	ev := events.Event{
		Event: "task_completed", Round: tw.round,
		TaskID: ob.TaskID, ToolCallID: ob.ToolCallID, TaskFile: taskFile,
	}
	copied := info // immutable copy: later deletion cannot erase it
	ev.Task = &copied
	// Format is a constant with no args: the journal carries identifiers only.
	tw.s.emitEnvelope(tw.job, &ev,
		"task completed (observed, plugin-reported; not independently verified)")
}

// publishFailure emits one diagnostic for an unconfirmed eligible completion.
func (tw *taskWatcher) publishFailure(ob client.Observation, kind fault.Kind) {
	ev := events.Event{
		Event: "task_lookup_failed", Round: tw.round,
		TaskID: ob.TaskID, ToolCallID: ob.ToolCallID,
		Reason: string(kind),
	}
	tw.s.emitEnvelope(tw.job, &ev, "task completion could NOT be confirmed: %s", kind)
}

// taskwatchReason maps the lookup's symbolic reason onto the fault vocabulary.
func taskwatchReason(why string) fault.Kind {
	switch why {
	case taskwatch.ReasonMissingStore:
		return fault.KindTaskStoreMissing
	case taskwatch.ReasonMemoryStore:
		return fault.KindTaskStoreMemory
	case taskwatch.ReasonInvalidData:
		return fault.KindTaskStoreInvalid
	case taskwatch.ReasonMissingTask:
		return fault.KindTaskMissing
	case taskwatch.ReasonAmbiguousID:
		return fault.KindTaskAmbiguous
	case taskwatch.ReasonNotCompleted:
		return fault.KindTaskNotCompleted
	case taskwatch.ReasonBadID:
		return fault.KindTaskBadInput
	default:
		return fault.KindTaskStoreInvalid
	}
}
