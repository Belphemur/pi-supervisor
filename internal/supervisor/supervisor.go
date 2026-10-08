// Package supervisor runs the per-job round loops: drives the daemon-native
// pi RPC client (internal/client), classifies exits, adaptive backoff, marker
// gate, instant-exit strikes.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"pi-supervisor/internal/client"
	"pi-supervisor/internal/events"
	"pi-supervisor/internal/fault"
	"pi-supervisor/internal/job"
	"pi-supervisor/internal/journal"
	"pi-supervisor/internal/stall"
	"pi-supervisor/internal/taskwatch"
)

// Handler is the control-socket surface (implemented by Supervisor).
type Handler interface {
	Status(job string) (any, error)
	Start(name string) error
	Stop(name string) error
	Steer(name, text string, noWait, interrupt bool) (job.SteerReport, error)
	Logs(name string, n int) ([]string, error)
	Reload() error
}

// interruptPID SIGINTs pi's process group so an in-flight turn is asked to
// stop and the steer frame becomes the next thing pi works on (ADR-0007).
//
// SIGINT targets `-pid` (the whole group), never the pi pid alone: the client
// spawns pi with Setpgid, and signaling the group reaches pi plus anything it
// spawned. It is deliberately SIGINT (not SIGTERM) so pi can abort the current
// generation and stay available for the steer, and it reports whether the
// signal was SENT — whether pi actually stops is pi's call, which is why the
// steer report marks it as requested, not obeyed.
func interruptPID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("no pi pid to interrupt")
	}
	return syscall.Kill(-pid, syscall.SIGINT)
}

// controlPollInterval is how often the client polls the ctrl file. Tests
// shrink it so a steer is delivered (and acked) in milliseconds; production
// uses the client's default.
var controlPollInterval = client.PollInterval

// steerWaitMargin is added to the poll interval to bound the ack wait: a
// frame is picked up within one poll, then acked in the same pass.
const steerWaitMargin = 2 * time.Second

// reportSteerMax is the ADR-0020 budget for "marker seen, report missing":
// the daemon asks twice, then closes the job fatal. A third ask would just
// burn rounds; the fatal names the missing artifact so the remedy is a
// copy-paste, not a diagnosis.
const reportSteerMax = 2

type Supervisor struct {
	mu   sync.Mutex
	jobs map[string]*runner
	stop chan struct{}
	wg   sync.WaitGroup
	// steerWait overrides the ack-wait budget; 0 = poll interval + margin.
	steerWait time.Duration
	// ghClients caches one GitHub client per owner/repo (ADR-0012). Lazy, so a
	// daemon that never reviews never touches GitHub credentials.
	ghClients *reviewClients
	// listOpen counts a PR's OPEN review threads for the post-completion
	// re-check. It is a field, not a direct package call, so it is bound to this
	// Supervisor in New() and therefore reuses ghClients instead of resolving a
	// fresh App installation token on every call.
	listOpen func(ctx context.Context, owner, repo string, pr int) (int, error)
	// reviewRounds is the default campaign budget (ADR-0012 §5); 0 = 5.
	reviewRounds int
	// reviewAckTimeoutDur bounds a pending bulk_resolve; 0 = 30m.
	reviewAckTimeoutDur time.Duration
	// reviewWarmupDur is how long the auto-trigger waits for CodeRabbit to
	// produce threads before counting; 0 = 5m.
	reviewWarmupDur time.Duration
}

type runner struct {
	mu       sync.Mutex
	job      job.Job
	state    job.State
	pid      int
	started  time.Time
	runlogB  int64
	lastText string // the round's streamed assistant text (ADR-0020 budget)
	stopCh   chan struct{}
	active   bool
	// stopSource records who asked for the CURRENT stop ("operator" via
	// Stop(), "daemon" via Shutdown) so the loop's classification points can
	// persist it and emit the right event (ADR-0017). Guarded by r.mu; set
	// BEFORE closing stopCh so the loop can never observe a closed channel
	// with an unset source.
	stopSource string
	// review is the live review campaign, if any (ADR-0012 §4). Separate from
	// state so a review round never resets the build job's accounting.
	review *reviewCampaign
	// autoReview is the one-shot intent to arm a review when the completion
	// gate closes on an open PR.
	autoReview *autoReviewSpec
	// pendingReportSteer arms an ADR-0020 report request: the marker was
	// seen on some surface but the final report is missing, so the NEXT live
	// round gets an abort+prompt asking for the report. In-memory on purpose:
	// the durable ask-count lives in state.ReportSteers (incremented on
	// confirmed delivery — qodo PR#7 finding 7), so a daemon restart loses
	// only the pending flag and the next boundary re-arms it. Cleared on
	// Stop/Restart so a fresh run never inherits an armed request (kody,
	// PR#7: it would steer "write the report" in round 1 before any work).
	pendingReportSteer bool
	// reportGraceRounds counts ADR-0020 grace rounds granted past the round
	// cap while a report request is still owed its delivery round. Bounded
	// by reportSteerMax independently of the ask count: a delivery that
	// keeps failing never increments state.ReportSteers (by design), so the
	// ask budget alone cannot bound the grace path.
	reportGraceRounds int
	// prScanner is the incremental PR-URL scanner for this job's transcript
	// (ADR-0006). Held per runner rather than recreated per gate, so the scrape
	// only ever reads bytes appended since the last call instead of re-reading a
	// growing file every round. Reset when the session path changes.
	prScanner *stall.PRURLScanner
	// launchedAt is when the current round spawned pi. It is the floor for
	// session discovery: a transcript that predates the spawn cannot be this
	// round's session, so FindSession must not adopt it. Zero on a RESUME
	// round, where the path is already pinned.
	launchedAt time.Time
	// sessWatch is the fsnotify session watcher armed for a LAUNCH round
	// (nil on RESUME). Round-scoped: armed in round() before the spawn, closed
	// when the round's client returns, so every exit path reaps it.
	sessWatch sessionResolver
}

func New() *Supervisor {
	s := &Supervisor{
		jobs:      map[string]*runner{},
		stop:      make(chan struct{}),
		ghClients: newReviewClients(),
	}
	// Bind the re-check seam to THIS supervisor so it reuses the per-repo
	// client CACHE (see listOpenThreads). Tests that stub the package var still
	// take effect, because s.listOpen defers to it.
	s.listOpen = func(ctx context.Context, owner, repo string, pr int) (int, error) {
		return listOpenThreads(s, ctx, owner, repo, pr)
	}
	return s
}

func (s *Supervisor) logf(name, format string, args ...any) {
	_, _, orchlog, _ := job.Paths(name)
	f, err := os.OpenFile(orchlog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// LoadJobs (re)reads the jobs dir; existing runners get hot-reloaded config.
func (s *Supervisor) LoadJobs() error {
	entries, err := os.ReadDir(job.JobsDir())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		j, err := job.Load(filepath.Join(job.JobsDir(), e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad job %s: %v\n", e.Name(), err)
			continue
		}
		seen[j.Name] = true
		if r, ok := s.jobs[j.Name]; ok {
			r.mu.Lock()
			r.job = j
			r.mu.Unlock()
			continue
		}
		r := &runner{job: j, stopCh: make(chan struct{})}
		if st, err := job.LoadState(j.Name); err == nil {
			r.state = st
			if r.state.State == "running" {
				r.state.State = "stopped" // adopt as resumable, not auto-running
			}
		}
		s.jobs[j.Name] = r
	}
	for name := range s.jobs {
		if !seen[name] {
			delete(s.jobs, name)
		}
	}
	return nil
}

// persistState snapshots the state under r.mu and writes it to disk.
//
// The write itself MUST happen while r.mu is still held. Taking the snapshot
// and then unlocking lets two persists interleave: the loop's
// persist-on-round-start (Round=N) and Stop()'s terminal write (State=stopped)
// both snapshot, then both write, and whichever lands last wins — so a stop
// during round 1 could persist Round:0 over the loop's Round=1. That is exactly
// the flake this fixes: TestStopTwiceIsSafe failed ~2 runs in 8 under -race
// because the file write was outside the lock.
//
// Cost: the atomic write (temp + fsync + rename) is held under the lock. That
// is acceptable because persistState is called on transitions, never in a hot
// loop, and correctness of the durable record outranks a few hundred
// microseconds of lock hold.
func (r *runner) persistState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = job.SaveState(r.job.Name, r.state)
}

func (r *runner) snapshot() job.Status {
	r.mu.Lock()
	st := job.Status{
		Name: r.job.Name, State: r.state.State, Round: r.state.Round,
		StopSource: r.state.StopSource,
		MaxRounds:  r.job.MaxRounds, SessionPath: r.job.SessionPath,
		ClientPID: r.pid, LastRC: r.state.LastRC, LastDurS: r.state.LastDurS,
		LastRunlogB: r.runlogB, InstantExits: r.state.InstantExits,
		CIStalls: r.state.CIStalls, ReportSteers: r.state.ReportSteers,
		LastDiag: r.state.LastDiag, PRURL: r.state.PRURL,
		LastUpdate: time.Now().Format(time.RFC3339),
	}
	if r.job.FinalReport != "" {
		st.FinalReportOK = job.Exists(r.job.FinalReport)
	}
	sessCopy, wtCopy := r.job.SessionPath, r.job.Worktree
	// Same for the post-completion thread baseline: persisted on State, and
	// copied here so `status` can actually show it.
	if r.state.ReviewBaseline != nil {
		bl := *r.state.ReviewBaseline
		st.ReviewBaseline = &bl
	}
	// Review snapshot is built INLINE: reviewStatus takes r.mu, which this
	// method already holds — calling it here would self-deadlock.
	if c := r.review; c != nil {
		c.mu.Lock()
		st.Review = &job.ReviewStatus{
			Active: r.active, Owner: c.owner, Repo: c.repo, PR: c.pr,
			Round: c.round, MaxRound: c.maxRound, Type: c.kind,
			PendingAcks: len(c.pendingAcks),
		}
		c.mu.Unlock()
	}
	if r.job.SessionPath != "" {
		if fi, err := os.Stat(r.job.SessionPath); err == nil {
			st.SessionBytes = fi.Size()
			st.SessionAgeS = time.Since(fi.ModTime()).Seconds()
		}
	}
	// Truthful mid-round, not just after classification (ADR-0011). Read under
	// the same lock as every other state field.
	st.MarkerFound = r.state.State == "done" || r.state.MarkerSeen
	r.mu.Unlock()
	// Task counts read ON DEMAND with NO lock held: one open-read of the
	// plugin's current store (owner amendment). Never a ticker, never a
	// second watcher, never derived from completion events.
	st.Tasks = taskProgress(sessCopy, wtCopy)
	return st
}

func (s *Supervisor) Status(name string) (any, error) {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return nil, fault.New(fault.KindUnknownJob, fmt.Errorf("unknown job %q", name))
	}
	return r.snapshot(), nil
}

// StatusAll returns snapshots for every job, sorted by name.
func (s *Supervisor) StatusAll() any {
	s.mu.Lock()
	names := make([]string, 0, len(s.jobs))
	byName := make(map[string]*runner, len(s.jobs))
	for n, r := range s.jobs {
		names = append(names, n)
		byName[n] = r
	}
	s.mu.Unlock()
	sort.Strings(names)
	out := make([]job.Status, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n].snapshot())
	}
	return out
}

func (s *Supervisor) Start(name string) error {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return fault.New(fault.KindUnknownJob, fmt.Errorf("unknown job %q", name))
	}
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return fault.New(fault.KindAlreadyRunning, fmt.Errorf("job %q already running", name))
	}
	// ADR-0016: `done` + an ARMED review campaign is the supported manual
	// re-entry (`review <job> --pr N` then `start`) — refusing here forced the
	// hand state-file deletion + daemon restart dance. The campaign owns the
	// loop from the first round: its budget (invariant 23) and its gate
	// (reviewGate) govern, and the marker gate must not classify done
	// mid-campaign (the sticky MarkerSeen latch of the PREVIOUS campaign would
	// otherwise close the resumed session instantly).
	if r.state.State == "done" && r.review == nil {
		r.mu.Unlock()
		return fault.New(fault.KindAlreadyDone, fmt.Errorf("job %q already done; clear state to rerun", name))
	}
	r.active = true
	// A live campaign always runs under `reviewing`, never `running`: the
	// loop's budget/gate selection keys off that state (ADR-0012 §4.2). A
	// manual arm used to leave `running` here, so the campaign could never
	// terminate itself and the job's MaxRounds applied instead.
	enteringReviewing := r.review != nil
	if enteringReviewing {
		r.state.State = "reviewing"
		// Mirror the auto handoff's stamp: the campaign's displayed round
		// starts at the state round it takes over from.
		r.review.round = r.state.Round
	} else {
		r.state.State = "running"
	}
	r.state.StopSource = "" // running again; the previous stop's source is spent
	r.state.InstantExits = 0
	if r.state.StartedAt == "" {
		r.state.StartedAt = time.Now().Format(time.RFC3339)
	}
	// Fresh stop channel every start: Stop() closed the previous one, and a
	// closed channel would make the new loop exit instantly.
	stopCh := make(chan struct{})
	r.stopCh = stopCh
	r.mu.Unlock()
	r.persistState()

	if enteringReviewing {
		// ADR-0016: say the handoff out loud. The auto path emits `reviewing`
		// at its done→reviewing hop; the manual re-entry must too, or a watch
		// client cannot tell that the loop now belongs to the campaign (and
		// `reviewing` is deliberately NOT terminal — the run is not over).
		r.mu.Lock()
		round := r.state.Round
		wt := r.job.Worktree
		r.mu.Unlock()
		s.emit(name, "reviewing", round, 0, 0, "",
			"manual review re-entry: the campaign owns the loop from here (worktree %s)", wt)
	}

	s.wg.Go(func() {
		s.loop(r, stopCh)
	})
	return nil
}

func (s *Supervisor) Stop(name string) error {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return fault.New(fault.KindUnknownJob, fmt.Errorf("unknown job %q", name))
	}
	r.mu.Lock()
	active := r.active
	if active {
		// Mark inactive synchronously under the lock so a second Stop() is a
		// no-op rather than a double-close of the stop channel.
		r.active = false
		// Operator stop (ADR-0017): recorded so a later watcher precheck
		// does NOT auto-resume — an explicit stop stays stopped.
		r.stopSource = "operator"
		close(r.stopCh)
		// Persist the terminal state HERE, not only in the loop's exit path.
		// The loop sets State=stopped when the round returns, but Stop() returns
		// BEFORE that happens, so a caller that reads the state file straight
		// after Stop() could see "running" — which also told a restarting daemon
		// the job was still live. The loop's later write is idempotent.
		//
		// Round floor: Stop() can land in the window between Start() (which
		// marks the job running) and the loop goroutine incrementing Round. A
		// stop in that window would persist Round:0, so a later start would
		// resume as if no round had ever run — and the durable record would
		// claim less progress than actually happened. Recording 1 keeps the
		// counter monotonic: the round was authorized and may have run.
		if r.state.Round < 1 {
			r.state.Round = 1
		}
		r.state.State = "stopped"
		r.state.StopSource = "operator"
		// Run-scoped report-request state must not leak into a later resume:
		// a pending ask armed at the previous boundary would steer "write the
		// report" into round 1 of the resumed run before any work (kody, PR#7).
		// The durable ask count (state.ReportSteers) is kept — the gate
		// re-arms from it at the first boundary if the signal is still there.
		r.pendingReportSteer = false
		r.reportGraceRounds = 0
	}
	pid := r.pid
	// Round the stop landed on, for the event payload. Read under the same lock
	// as the mutation above so it cannot disagree with what was persisted.
	stoppedAtRound := r.state.Round
	r.mu.Unlock()
	if !active {
		return fault.New(fault.KindNotRunning, fmt.Errorf("job %q not running", name))
	}
	r.persistState()
	r.closeSessWatch() // an operator stop must not leave the round's watcher armed
	// The loop emits `stopped` from its own exit path, but Stop() can win the
	// race before the loop's first stopCh check — in which case no event is ever
	// emitted and a `watch` client blocks until its timeout on a job that is
	// already stopped. Emit it here, and let the loop's own emit be the
	// duplicate: watchers treat the first terminal event as the answer and close,
	// so a second one is harmless, whereas a missing one is a hang.
	s.emit(name, "stopped", stoppedAtRound, 0, 0, "", "operator stop")
	s.logf(name, "stop requested by operator (pid %d)", pid)
	if pid <= 0 {
		// The client may not have published its pid yet (still spawning).
		// The round goroutine's stop path escalates on its own; nothing to
		// signal here.
		return nil
	}
	// Kill the whole process group, then escalate if it lingers. Bounded so
	// Stop() never returns while pi is still alive (an orphan pi would keep
	// writing to the session JSONL unsupervised).
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		gone := r.pid == 0
		r.mu.Unlock()
		if gone {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return nil
}

// Restart performs an atomic fresh restart (restart --fresh, ADR-0010):
//
//  1. Stop the live round (SIGTERM→SIGKILL with 3s grace), waiting for the
//     pi process group to be fully reaped — never returns while pi survives.
//  2. Quarantine the session JSONL → _archived-stale/ with a timestamp
//     suffix (preserves the audit trail; bytes preserved by os.Rename).
//  3. Clear the job's session_path + round counter in state, so the next
//     round LAUNCHes a brand-new session instead of re-adopting the old one.
//  4. Start a fresh round (spawns pi with a new session, brief + cont read
//     from disk at spawn time).
//
// The whole sequence is guarded under r.mu for steps 2-3 so FindSession cannot
// observe a half-quarantined state and re-adopt the file between steps.
func (s *Supervisor) Restart(name string) error {
	if err := s.Stop(name); err != nil {
		// "not running" is not a stop failure for a restart — proceed to
		// clear state and start fresh.
		if !strings.Contains(err.Error(), "not running") {
			return fmt.Errorf("restart %s: stop: %w", name, err)
		}
	}
	s.mu.Lock()
	r, ok := s.jobs[name]
	if !ok {
		s.mu.Unlock()
		return fault.New(fault.KindUnknownJob, fmt.Errorf("unknown job %q", name))
	}
	r.mu.Lock()
	r.state.Round = 0
	// A fresh restart is a NEW RUN: the previous run's completion latch,
	// report-ask count, and start floor must not leak into it — a stale
	// marker latch plus the previous run's report would otherwise close the
	// fresh run done at round 1 before a single round of work (ADR-0020).
	r.state.MarkerSeen = false
	r.state.ReportSteers = 0
	r.state.StartedAt = ""
	// Run-scoped in-memory report-request state: a fresh run must not
	// inherit an armed ask or spent grace budget (kody, PR#7).
	r.pendingReportSteer = false
	r.reportGraceRounds = 0
	// Quarantine a report left by the OLD run the same way the transcript is
	// quarantined (qodo PR#7 finding 4 / kody): without this, a report
	// written shortly before the restart satisfies the mtime floor and the
	// fresh run closes done at round 1 on the previous run's deliverable.
	// Move-only like job.Quarantine — bytes preserved next to the old file.
	if rep := r.job.FinalReport; rep != "" {
		if _, err := os.Stat(rep); err == nil {
			stale := filepath.Join(filepath.Dir(rep), "_archived-stale")
			if err := os.MkdirAll(stale, 0o755); err == nil {
				dst := filepath.Join(stale, filepath.Base(rep)+"_"+time.Now().Format("20060102T150405"))
				if err := os.Rename(rep, dst); err == nil {
					s.logf(name, "restart --fresh: quarantined previous report %s -> %s", rep, dst)
				}
			}
		}
	}
	oldSession := r.job.SessionPath
	r.job.SessionPath = ""
	adopted := r.job
	state := r.state
	r.mu.Unlock()
	s.mu.Unlock()

	// Quarantine the old transcript out of the live session set.
	var qPath string
	var qErr error
	if oldSession != "" {
		qPath, qErr = job.Quarantine(oldSession)
	}
	// Clear persisted state so the round counter restarts and FindSession
	// sees no session (forces a fresh LAUNCH). save atomically.
	if err := job.SaveState(name, state); err != nil {
		return fmt.Errorf("restart %s: persist state: %w", name, err)
	}
	if err := job.Save(adopted); err != nil {
		return fmt.Errorf("restart %s: persist job: %w", name, err)
	}
	if qErr != nil {
		return fmt.Errorf("restart %s: quarantine %s: %w", name, oldSession, qErr)
	}
	s.logf(name, "restart --fresh: quarantined %s -> %s, round reset to 0", oldSession, qPath)
	s.emit(name, "restart_fresh", 0, 0, 0, "",
		"fresh restart: quarantined %s, round counter reset", qPath)

	return s.Start(name)
}

// Steer sends one control frame to a live round and reports what pi did with
// it (ADR-0005). Prose is wrapped in a prompt frame; a caller-supplied JSON
// frame is passed through unchanged. The write is only a request: the real
// outcome comes from the client's ack record for this frame id, and the wait
// for it holds no supervisor lock so the round loop keeps running.
func (s *Supervisor) Steer(name, text string, noWait, interrupt bool) (job.SteerReport, error) {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return job.SteerReport{Job: name}, fault.New(fault.KindUnknownJob, fmt.Errorf("unknown job %q", name))
	}
	frame, id, err := steerFrame(text)
	if err != nil {
		return job.SteerReport{Job: name}, err
	}

	// Snapshot under r.mu, then let go: nothing below may hold s.mu or r.mu
	// across a wait (lock order is s.mu -> r.mu, and the round loop needs
	// r.mu to publish its own end).
	r.mu.Lock()
	rep := job.SteerReport{
		Job:         name,
		FrameID:     id,
		Round:       r.state.Round,
		JobState:    r.state.State,
		SessionPath: r.job.SessionPath,
		CtrlPath:    job.Ctrl(name),
		AckPath:     job.Ack(name),
		LiveRound:   r.active && r.pid > 0,
		ClientPID:   r.pid,
	}
	r.mu.Unlock()

	if !rep.LiveRound {
		// Do not queue it: round() truncates the ctrl file at the start of
		// every round, so a frame written now would be wiped unread.
		rep.Outcome = job.AckNoRound
		rep.Detail = fmt.Sprintf("job state %q, round %d: no round is reading %s, so nothing was written",
			rep.JobState, rep.Round, rep.CtrlPath)
		return rep, fault.New(fault.KindNoLiveRound, fmt.Errorf("%s", rep.Detail))
	}

	// Remember where the ack log ends so only records the client writes for
	// THIS frame are considered (a restart reuses the same file).
	off := job.Size(rep.AckPath)

	// Interrupt mode: ask pi to stop its current turn BEFORE the frame lands,
	// so the steer is what it picks up next (ADR-0007). Signaling first is
	// what makes it an interrupt rather than a queued message — a steer that
	// only queues would be answered after the turn already in flight.
	//
	// A failed signal is a failure, not a warning: the operator asked for the
	// running turn to be dropped, and silently degrading to a plain queued
	// steer would be exactly the "looks like it worked, wasn't" outcome ADR-0005
	// exists to prevent. The frame is NOT written in that case — no point
	// queueing a steer the operator would read as an interrupt.
	if interrupt {
		r.mu.Lock()
		pid := r.pid
		r.mu.Unlock()
		if err := interruptPID(pid); err != nil {
			rep.Outcome = job.AckSendFail
			rep.Detail = fmt.Sprintf("interrupt requested but cannot SIGINT pi group (-%d): %v; frame not written", pid, err)
			return rep, fmt.Errorf("%s", rep.Detail)
		}
		rep.Interrupted = true
		s.logf(name, "steer --interrupt: SIGINT to pi group -%d (round %d)", pid, rep.Round)
		// No sleep here on purpose: the client owns the abort-drain handshake.
		// A frame that lands mid-abort is held by that handshake and forwarded
		// when the aborted turn ends, which the report surfaces as
		// held-then-forwarded. Sleeping would only delay the delivery the
		// client is already scheduling correctly.
	}

	if err := appendCtrl(rep.CtrlPath, frame); err != nil {
		rep.Outcome = job.AckSendFail
		rep.Detail = fmt.Sprintf("cannot write %s: %v", rep.CtrlPath, err)
		return rep, fmt.Errorf("%s", rep.Detail)
	}
	if noWait {
		rep.Outcome = job.AckWritten
		rep.Detail = "--no-wait: on disk, not confirmed; check the run log for \"[control] forwarded\""
		return rep, nil
	}
	s.waitAck(&rep, r, off)
	if !rep.Confirmed {
		return rep, fmt.Errorf("frame %s not confirmed: %s", rep.FrameID, rep.Outcome)
	}
	return rep, nil
}

// waitAck polls the ack log for this frame's terminal record. It never holds
// a supervisor lock, and it reports the round as gone rather than success
// when the round ends first.
func (s *Supervisor) waitAck(rep *job.SteerReport, r *runner, off int64) {
	wait := s.steerWait
	if wait <= 0 {
		wait = controlPollInterval + steerWaitMargin
	}
	start := time.Now()
	held := false
	for {
		for _, rec := range acksSince(rep.AckPath, off) {
			if rec.ID != rep.FrameID {
				continue
			}
			if rec.Outcome == job.AckHeld {
				// Accepted but not delivered yet: keep waiting for the
				// terminal record that follows the abort drain.
				held = true
				continue
			}
			rep.Outcome, rep.Detail, rep.DelayMS, rep.Confirmed = rec.Outcome, rec.Detail, rec.DelayMS, true
			rep.WaitedMS = time.Since(start).Milliseconds()
			if held {
				rep.Detail = strings.TrimSpace(rep.Detail + " (accepted while an abort drained, delivered after)")
			}
			return
		}
		if time.Since(start) >= wait {
			break
		}
		// The ack log is truncated with the ctrl file at every round start:
		// shrinking below our offset means this round ended.
		if job.Size(rep.AckPath) < off {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	rep.WaitedMS = time.Since(start).Milliseconds()
	rep.Outcome = job.AckUnconfirmed
	if r.live() {
		rep.Detail = fmt.Sprintf("no ack from pi within %s; the frame is on disk but unconfirmed", wait)
	} else {
		rep.Detail = fmt.Sprintf("round %d stopped reading %s before the frame was delivered; "+
			"the next round truncates it, so re-send after it starts", rep.Round, rep.CtrlPath)
	}
}

// acksSince decodes the ack records appended at or after offset.
func acksSince(path string, off int64) []job.AckRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(off, 0); err != nil {
		return nil
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	var out []job.AckRecord
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec job.AckRecord
		if json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// appendCtrl adds one LF-terminated JSON frame to the steer file.
func appendCtrl(path string, frame map[string]any) error {
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

var steerSeq atomic.Uint64

// steerFrame turns operator text into one control frame. Prose becomes a
// prompt frame (the client's contract is strict JSON-LF); a caller-supplied
// JSON object with a "type" is passed through so operators can still send an
// abort or any other frame type by hand. Every frame gets an id so its ack
// record can be matched back.
func steerFrame(text string) (map[string]any, string, error) {
	body := strings.TrimSpace(text)
	if body == "" {
		return nil, "", errors.New("empty steer text")
	}
	var frame map[string]any
	if strings.HasPrefix(body, "{") && json.Unmarshal([]byte(body), &frame) == nil {
		if _, ok := frame["type"].(string); !ok {
			frame = nil // JSON but not a frame: treat it as prose
		}
	} else {
		frame = nil
	}
	if frame == nil {
		frame = map[string]any{"type": "prompt", "message": body}
	}
	id, _ := frame["id"].(string)
	if id == "" {
		id = fmt.Sprintf("steer-%d-%d", time.Now().UnixNano(), steerSeq.Add(1))
		frame["id"] = id
	}
	return frame, id, nil
}

func (s *Supervisor) Logs(name string, n int) ([]string, error) {
	if n <= 0 {
		n = 30
	}
	return job.LastLines(job.Runlog(name), n)
}

// Reload rescans the jobs dir (hot config).
func (s *Supervisor) Reload() error { return s.LoadJobs() }

// loop is the round loop for one job.
func (s *Supervisor) loop(r *runner, stopCh chan struct{}) {
	for {
		// An operator/daemon stop that landed between Start() and this
		// goroutine's first step already closed stopCh AND applied the round
		// floor (state.Round >= 1). Without this check the cap classification
		// below runs on the floor value: a 1-round job stopped pre-round read
		// round=2 > maxRounds=1 and fired "round cap reached without marker",
		// overwriting the stopped state with fatal (TestWatchSeesStoppedOn
		// OperatorStop leaked exactly that fatal after its clean stop).
		// A stop is terminal: classify nothing, return silently — Stop() has
		// already persisted the terminal state and emitted the event.
		//
		// The check reads stopCh UNDER r.mu, the same lock Stop() and
		// Shutdown() hold while they close the channel and apply the round
		// floor (qodo PR#7 finding 2). A pre-lock check left a TOCTOU gap:
		// stop lands between the check and the read, the loop classifies the
		// floored counter (round=2 > maxRounds=1) and fires the cap fatal
		// over the freshly persisted stopped state. Under the lock the two
		// sides are serialized — a closed channel is always seen before any
		// classification on floored state.
		r.mu.Lock()
		round := r.state.Round + 1
		maxRounds := r.job.MaxRounds
		// reviewing is read in two places below (the round cap and the
		// campaign gate), so it is computed once here.
		var reviewing bool
		// While a review campaign is live it owns the budget (ADR-0012 §4.2):
		// the build job's MaxRounds is a different, larger number, and using it
		// would let a review campaign run ~200 rounds instead of its own 5.
		if c := r.review; c != nil && r.state.State == "reviewing" {
			reviewing = true
			c.mu.Lock()
			if c.maxRound > 0 {
				maxRounds = c.maxRound
			}
			c.mu.Unlock()
		}
		select {
		case <-stopCh:
			r.mu.Unlock()
			return
		default:
		}
		r.mu.Unlock()
		// A campaign's budget counts ROUNDS WITH WORK, not raw loop rounds
		// (ADR-0012 §4.2 amendment): a provider that returns an empty
		// 0-token response — flambette#65 rounds 5-6, 3-4s each, nothing
		// done — must not burn the operator's round budget on nothing. The
		// loop increments c.spent below ONLY when a round produced output
		// (runlog > 0); exhaustion is spent >= maxRound. Build jobs keep
		// the raw round > maxRounds cap (their rounds are cheap and the
		// marker/report gate governs them).
		campaignExhausted := false
		if reviewing {
			// Snapshot the campaign pointer UNDER r.mu (kody PR#9 finding 3):
			// r.review is written by StartReview/ArmAutoReview/finish/reviewGate
			// under r.mu, so an unlocked read here races those writes.
			r.mu.Lock()
			c := r.review
			r.mu.Unlock()
			if c != nil {
				c.mu.Lock()
				// Two exhaustion conditions, BOTH campaign-scoped: the
				// budget is SPENT on real rounds, or the campaign's own
				// raw round count ran to 3× the budget — the backstop
				// against a provider that returns rc=0 empty responses
				// forever (free rounds must not mean infinite rounds).
				// r.state.Round is deliberately NOT used: it is the job's
				// lifetime counter and is never reset on the
				// build→reviewing transition, so a long build would
				// exhaust a fresh campaign before its first round.
				campaignExhausted = c.maxRound > 0 &&
					(c.spent >= c.maxRound || c.rawRound >= c.maxRound*3)
				c.mu.Unlock()
			}
		}
		if campaignExhausted || (!reviewing && round > maxRounds) {
			r.mu.Lock()
			name := r.job.Name
			r.mu.Unlock()
			// A campaign that exhausts its budget is a distinct failure from a
			// build job that never reached its marker, and the message has to
			// say which: "round cap reached without marker" would be a lie for
			// a review round (there is no marker in a review round).
			if reviewing {
				// Snapshot the campaign identity FIRST: finish() clears the
				// campaign (ADR-0016), and the baseline needs its owner/repo/pr
				// and last observed open count.
				c := r.campaignSnapshot()
				r.finish("fatal", "review round cap reached with threads still open")
				s.logf(name, "FATAL: review round cap %d reached without a clean review", maxRounds)
				s.emit(name, "review_exhausted", round, 0, 0, "",
					"review round cap %d reached with threads still open — re-arm with `pi-supervisor review %s --pr N`",
					maxRounds, name)
				// Record the baseline on THIS path too. An exhausted campaign
				// ends with threads STILL OPEN, and that count is the baseline:
				// without it the next re-check has nothing to compare against,
				// and every still-open thread reads as brand new. Recorded AFTER
				// finish(), because the re-check no-ops while the job is active.
				if c != nil {
					s.recordThreadBaseline(name, r, c.owner, c.repo, c.pr, c.lastOpenCount())
					// Same reason as the clean-close path: finish() has cleared
					// active and a baseline now exists, so this is a point where
					// the re-check can run rather than no-op.
					go s.recheckThreads(name)
				}
				return
			}
			// ADR-0020 grace round (qodo PR#7 finding 6): a report request
			// armed at the previous boundary still owes the agent its
			// delivery round. The marker appearing in the LAST permitted
			// round armed the ask after that round ended, and the delivery
			// goroutine only runs inside a live round — the cap would
			// fatal here before the ask ever reached the agent. Grant the
			// delivery round instead of fataling. The grant is bounded
			// twice: by the ask budget (asks stop being granted once
			// ReportSteers reaches reportSteerMax) and by its own counter
			// (a delivery that keeps failing never increments the ask
			// count, so the ask budget alone cannot bound this path).
			r.mu.Lock()
			pending, asked := r.pendingReportSteer, r.state.ReportSteers
			grant := pending && asked < reportSteerMax && r.reportGraceRounds < reportSteerMax
			if grant {
				r.reportGraceRounds++
			}
			r.mu.Unlock()
			if grant {
				s.logf(name, "round %d: past cap with report request #%d owed its delivery round — grace granted", round, asked+1)
				// Fall through: the delivery goroutine is spawned with the
				// round, and the gate below re-arms or fatals at the
				// boundary, so the budget stays closed end-to-end.
			} else {
				// Same honesty requirement for the build job. `done` requires BOTH
				// the marker AND the final report (ADR-0011), so "without marker"
				// is only true in one of three cases. Reporting it unconditionally
				// sent an operator hunting a marker that had in fact been found:
				// mealime-extracats3 sat at fatal/14 with marker_seen=true and
				// last_diag="round cap reached without marker", because the brief
				// told pi to write /tmp/mealime_extracats_final_report.md while the
				// job's final_report was /tmp/mealime_extracats3_final_report.md.
				// Thirteen rounds were burned re-running finished work, and the
				// diagnostic pointed at the wrong cause the whole time.
				//
				// Name the ACTUAL missing artifact so the next run is one copy-paste
				// instead of a diagnosis.
				r.mu.Lock()
				report := r.job.FinalReport
				r.mu.Unlock()
				// ONE funnel with the completion gate (DRY): the two must never
				// disagree about what "seen" means.
				markerSeen := s.markerSeenNow(r)
				switch {
				case !markerSeen && report != "" && !job.Exists(report):
					msg := fmt.Sprintf("round cap %d reached: marker NOT seen AND final report missing (%s)%s",
						maxRounds, report, s.markerSurfaceDiag(r))
					r.finish("fatal", msg)
					s.logf(name, "FATAL: %s", msg)
					s.emit(name, "fatal", round, 0, 0, "", "%s", msg)
				case !markerSeen:
					msg := fmt.Sprintf("round cap %d reached without marker%s", maxRounds, s.markerSurfaceDiag(r))
					r.finish("fatal", msg)
					s.logf(name, "FATAL: %s", msg)
					s.emit(name, "fatal", round, 0, 0, "", "%s", msg)
				case report == "":
					// Marker found and no report was ever configured: `done` is
					// reachable, so the cap with the marker latched means the gate
					// could not close — say that rather than blaming the marker.
					msg := fmt.Sprintf("round cap %d reached with marker seen but no final_report configured — check the job's final_report path", maxRounds)
					r.finish("fatal", msg)
					s.logf(name, "FATAL: %s", msg)
					s.emit(name, "fatal", round, 0, 0, "", "%s", msg)
				default:
					// The expensive one: the agent DID its job and said so, and the
					// report is simply somewhere else. Look for it, so the remedy is
					// a copy-paste of a path we PRINT rather than a hunt through
					// /tmp. mealime-extracats3 burned 13 rounds to an operator
					// guess that took one `ls`.
					msg := fmt.Sprintf("round cap %d reached: marker WAS seen but final report is missing at %s", maxRounds, report)
					// Floor = the RUN's StartedAt, not the current round's
					// spawn time: the grace rounds (and any multi-round run)
					// re-arm r.started every round, so a report the agent
					// wrote in round 1 was filtered as "stale" by a round-4
					// lookup (TestCapDiagnosticFindsMisnamedReport). Zero
					// time (unknown run start) disables the filter, matching
					// FindReportNearby's own contract.
					var runStart time.Time
					if t0, err := time.Parse(time.RFC3339, r.stateSnapshot().StartedAt); err == nil {
						runStart = t0
					}
					if near := job.FindReportNearby(report, runStart); near != "" {
						msg += fmt.Sprintf(" — FOUND at %s instead; the brief and the job's final_report disagree. Copy it to %s (or fix the brief) and re-arm", near, report)
					} else {
						msg += " — the agent may have written it elsewhere; check the brief's stated path"
					}
					r.finish("fatal", msg)
					s.logf(name, "FATAL: %s", msg)
					s.emit(name, "fatal", round, 0, 0, "", "%s", msg)
				}
				return
			}
		}
		r.mu.Lock()
		r.state.Round = round
		j := r.job
		r.mu.Unlock()
		// Persist the round counter as soon as the round starts. Two reasons:
		// a daemon crash mid-round must not resume as if the round never
		// happened, and Stop() reads this value when it writes the terminal
		// state — without it, a stop during round 1 persists Round:0.
		r.persistState()
		s.logRound(j.Name, "round_start", round, 0, 0,
			"round %d/%d", round, maxRounds)
		if round == 1 {
			s.emit(j.Name, "job_started", round, 0, 0, "", "round 1 launched")
		}
		// CI-stall watcher (ADR-0004): rounds with a captured transcript get
		// a detector that interrupts the session and fails the run once the
		// agent has parked on the CI/review loop ci_stall_cap times.
		r.mu.Lock()
		sess := r.job.SessionPath
		marker := r.job.Marker
		r.mu.Unlock()
		var rc int
		var dur, runlogB int64
		// Completion detection (ADR-0011). The marker lives in pi's ASSISTANT
		// MESSAGES in the session transcript, streamed live while the round
		// runs — NOT in the run log, which round() truncates at the start of
		// every round. Reading the run log made a cumulative question ("has
		// this job ever finished?") unanswerable: the evidence was destroyed
		// each round, so mealime-roomux burned 14 rounds on already-finished
		// work and ended fatal.
		//
		// The watcher starts even on a fresh LAUNCH (sess == ""), because pi
		// creates the transcript as it runs; it re-resolves the path itself.
		markerStop, markerExited := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(markerExited)
			s.watchMarker(r, sess, marker, round, markerStop, stopCh)
		}()

		// ADR-0020 report-request delivery: armed by the gate when the
		// completion signal arrived without a report. No-op when nothing is
		// pending; round-scoped like every other watcher.
		steerStop, steerExited := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(steerExited)
			s.deliverPendingReportSteer(r, round, steerStop, stopCh)
		}()

		// Mid-round thread reminders (ADR-0019) run for EVERY round of a
		// campaign — steer delivery needs a live round, not a transcript
		// path, so round 1 of a fresh LAUNCH is covered too. No campaign
		// armed: the watcher exits at once (no-op).
		remStop, remExited := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(remExited)
			s.watchThreadReminders(r, round, remStop, stopCh)
		}()
		if sess != "" {
			watchStop, watchExited := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(watchExited)
				s.watchCIStalls(r, sess, round, watchStop, stopCh)
			}()
			emptyStop, emptyExited := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(emptyExited)
				s.watchEmptyTurn(r, sess, round, emptyStop, stopCh)
			}()
			rc, dur, runlogB = s.round(r, round, stopCh)
			close(watchStop)
			close(emptyStop)
			<-watchExited // watcher stopped before the round is classified
			<-emptyExited // empty-turn watcher likewise
		} else {
			rc, dur, runlogB = s.round(r, round, stopCh)
		}
		close(remStop)
		<-remExited // reminder watcher likewise
		close(steerStop)
		<-steerExited // report-request delivery likewise
		close(markerStop)
		<-markerExited // completion watcher likewise
		// Adopt the transcript a fresh LAUNCH just created, so the gate below
		// has a surface to read on the very round that started the session.
		if sess == "" {
			if found := s.captureSession(r); found != "" {
				sess = found
				r.mu.Lock()
				r.job.SessionPath = found
				r.mu.Unlock()
				_ = job.Save(r.job)
			}
		}
		// Budget accounting: a campaign round counts against the budget only
		// when the agent actually produced something — assistant TEXT on the
		// round's result, not the runlog size (the runlog also mirrors client
		// diagnostics, so a forwarded steer's diag line would count an empty
		// turn as work — qodo PR#9 finding 3). An empty round (provider
		// returned a 0-token response) is free — the operator's budget is for
		// work, not for flakes. The raw counter bumps regardless: the 3x
		// backstop must bound even all-empty rounds. The campaign pointer is
		// snapshotted under r.mu (kody PR#9 finding 3).
		if reviewing {
			r.mu.Lock()
			produced := strings.TrimSpace(r.lastText) != ""
			c := r.review
			r.mu.Unlock()
			if c != nil {
				c.mu.Lock()
				c.rawRound++
				if produced {
					c.spent++
				}
				c.mu.Unlock()
			}
		}
		select {
		case <-stopCh:
			r.mu.Lock()
			src := r.stopSource // "operator" or "daemon" (ADR-0017); set before close
			r.state.State, r.state.LastRC, r.state.StopSource = "stopped", rc, src
			r.active = false
			name := r.job.Name
			r.mu.Unlock()
			r.persistState()
			r.closeSessWatch()
			s.logf(name, "loop stopped at round %d (client rc=%d, stop source=%s)", round, rc, src)
			// Daemon shutdown is not an operator stop: the info string feeds
			// the watch footer, and "operator" would misread as intent.
			info := "operator stop"
			if src == "daemon" {
				info = "daemon shutdown — a re-arming watcher will resume this job"
			}
			s.emit(name, "stopped", round, rc, dur, "", "%s", info)
			return
		default:
		}

		r.mu.Lock()
		r.state.LastRC, r.state.LastDurS = rc, dur
		name := r.job.Name
		instant := rc != 0 && dur < 60 && runlogB < 4096
		if instant {
			r.state.InstantExits++
		} else if rc == 0 || dur >= 1700 {
			r.state.InstantExits = 0
		}
		strikes := r.state.InstantExits
		r.state.LastDiag = job.Tail(job.Runlog(name), 240)
		r.mu.Unlock()

		snap := r.stateSnapshot()
		roundText := snap.LastDiag
		if snap.PRURL != "" {
			roundText += " | pr " + snap.PRURL
		}
		s.emit(name, "round_done", round, rc, dur, roundText, "")
		if instant {
			s.emit(name, "instant_exit", round, rc, dur, "", "instant exit strike %d/3 (rc=%d, %ds)", strikes, rc, dur)
		}
		// The post-completion thread re-check is NOT fired here. This point is
		// still inside the round, with r.active==true, and recheckThreads
		// no-ops on an active job (a live campaign's own replies would be
		// miscounted as new findings) — so a call from here, or from round
		// start, would never perform a check. It fires from the completion
		// gate instead, right after active goes false.
		s.logf(name, "round %d: client exit=%d duration=%ds runlog=%dB", round, rc, dur, runlogB)
		if r.stateSnapshot().LastDiag != "" {
			s.logf(name, "round %d diagnostic: %s", round, r.stateSnapshot().LastDiag)
		}

		// closedJob records that THIS round ended the build job, which is the
		// only moment the post-completion review re-check may run.
		closedJob := false
		// The marker gate (ADR-0011). The completion signal comes from the
		// SESSION TRANSCRIPT — the streamed MarkerSeen latch, plus a direct
		// scan as a belt-and-braces for a marker written after the last tick.
		//
		// It is NOT the run log: round() truncates /tmp/pi_<job>_run.log at
		// the start of every round, so a marker emitted in an earlier round is
		// structurally invisible there. That made mealime-roomux burn 14
		// rounds and end fatal ("round cap reached without marker") with the
		// work finished and PR #43 open.
		//
		// An empty marker would make any "contains" check match ANY non-empty
		// text (strings.Contains(x, "") is true), so a job configured without a
		// marker could be declared done by a stale final report. Refuse: no
		// marker, no done.
		//
		// TWO surfaces, OR'd on purpose:
		//
		//  1. the sticky MarkerSeen latch (transcript, streamed live), and
		//  2. the cumulative transcript scan (belt-and-braces for a marker
		//     written after the last tick), and
		//  3. the CURRENT round's run log — the weaker legacy surface, kept
		//     because an agent can legitimately announce completion on stdout
		//     without persisting an assistant record.
		//
		// The run log alone can never be sufficient across rounds (it is
		// truncated each round, which is the bug), but within the round that
		// produced it, it is a valid signal and costs nothing.
		// ONE funnel with the cap switch (DRY): the sticky transcript latch,
		// the cumulative transcript scan, and the current round's run log.
		markerSeen := s.markerSeenNow(r)
		// Scrape the PR URL from the transcript at the gate, not only from the
		// stall watchers (ADR-0006). Those watchers start only once a transcript
		// path is pinned, so on a fresh LAUNCH — where the round that opens the
		// PR and the round that finishes it are often the SAME one — nothing was
		// scraping. autoReviewHandoff then saw an empty prURL and emitted
		// review_skipped ("no GitHub PR was linked") for a transcript that
		// plainly contained the link.
		//
		// "" still means "not linked", never "no PR exists", so a genuine miss
		// stays a skip rather than becoming an error.
		if r.stateSnapshot().PRURL == "" && sess != "" {
			r.mu.Lock()
			if r.prScanner == nil || r.prScanner.Path() != sess {
				r.prScanner = stall.NewPRURLScanner(sess)
			}
			sc := r.prScanner
			r.mu.Unlock()
			if u := sc.PRURL(); u != "" {
				s.recordPR(r, u)
			}
		}

		// A report that DECLARES itself incomplete is evidence AGAINST
		// completion, not for it. This is the second signal the
		// mealime-userrecipes false positive had available and ignored: the
		// report said "**Status: PARTIAL.**", "T6-T9 are NOT done" and
		// "Repo facts a follow-up run needs", while the gate checked only that
		// the file EXISTED.
		//
		// Per the decision on that incident: never silently stop on a partial.
		// The job does NOT go done — it keeps looping so the next round can
		// finish the work — and the event says exactly why, quoting the agent's
		// own words. Silently marking it done is the failure mode that lost
		// four rounds of real work.
		if phrase, partial := job.ReportDeclaresIncomplete(j.FinalReport); partial {
			r.mu.Lock()
			r.state.State, r.active = "running", true
			r.state.LastDiag = fmt.Sprintf(
				"report declares the work INCOMPLETE (%q) — not marking done, continuing to the next round", phrase)
			name := r.job.Name
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "round %d: report declares incomplete (%q) — continuing", round, phrase)
			s.emit(name, "report_incomplete", round, rc, dur, "",
				"the final report at %s declares the work INCOMPLETE (%q) and the marker %q was not emitted as a standalone line, "+
					"so this job is NOT done — it continues to the next round. The agent is documenting unfinished work; "+
					"the report is at %s",
				j.FinalReport, phrase, j.Marker, j.FinalReport)
			// `continue`, NOT `return`: this is inside loop(), whose only caller
			// hands it to a WaitGroup. Returning would end the round goroutine
			// with state still running/active=true, leaving RunningCount stuck
			// at 1 forever and no further round ever starting — the job would
			// hang instead of continuing.
			continue
		}

		// ADR-0016: the marker gate is INERT while a live campaign owns the
		// loop (`reviewing`). The sticky MarkerSeen latch plus the cumulative
		// transcript scan find the PREVIOUS campaign's marker in a resumed
		// session and would close the job done at the end of round 1 — the
		// live mealime-rebase54 failure. A review round has no marker of its
		// own; its exits are reviewGate (0 threads && CI pass) and
		// review_exhausted only. The AUTO trigger path keeps this gate: there
		// r.review is still nil (the handoff below CREATES the campaign).
		//
		// ADR-0020 (supersedes the ADR-0011 marker AND report conjunction):
		// the fully written final report IS the completion signal. The marker
		// stays as the streaming early-warning and as the trigger for a
		// report request, but a report that exists, does not declare itself
		// incomplete (checked above), and was written during this run closes
		// the job done on its own. The marker is a completion INTENT: when it
		// appears anywhere but the report never does, the daemon asks for the
		// report — twice — and then closes fatal rather than burning rounds.
		startedAt := r.stateSnapshot().StartedAt
		reportOK := j.FinalReport != "" && reportReady(j.FinalReport, startedAt)
		switch {
		case !reviewing && j.Marker != "" && reportOK:
			// ADR-0012 §4.1: the auto-trigger runs HERE, in the gate's tail —
			// not "mid-round". There is no live round to fire from: the gate
			// clears `active` below and Start refuses a done job. So the
			// handoff is explicit: the build job closes as done, and if an
			// auto_review stanza is armed against an OPEN PR, the job
			// transitions done -> reviewing and keeps looping on the SAME
			// session (never re-LAUNCHing).
			autoArmed := s.autoReviewHandoff(name, r)

			r.mu.Lock()
			if autoArmed {
				// reviewing: the campaign owns the loop from here. The build
				// job's done verdict is preserved on state for the final
				// report; only the live verdict changes.
				r.state.State = "reviewing"
				r.active = true
				r.review.round = r.state.Round
			} else {
				r.state.State, r.active = "done", false
				// Done AND inactive: the only point the post-completion
				// re-check can actually run (ADR-0012 follow-up). Started as a
				// goroutine AFTER the lock is released below, since
				// recheckThreads takes r.mu itself.
				closedJob = true
			}
			r.mu.Unlock()
			r.persistState()
			if closedJob {
				go s.recheckThreads(name)
			}
			if markerSeen {
				s.logf(name, "round %d: marker %q detected in session transcript — done", round, j.Marker)
			} else {
				s.logf(name, "round %d: final report %s complete — done (ADR-0020)", round, j.FinalReport)
			}
			if autoArmed {
				// Emit `reviewing`, NOT `done`. `done` is a TERMINAL event
				// (events.Event.Terminal), so a `watch` client would print
				// "THE RUN IS OVER", exit 0, and stop listening while the
				// campaign is still running. The build verdict is preserved
				// on state for the final report; the LIVE verdict is the
				// campaign's.
				s.emit(name, "reviewing", round, 0, dur, "",
					"completion gate closed (marker=%t, report=%s) — build job done, entering the review campaign (ADR-0012 §4.1, ADR-0020)",
					markerSeen, j.FinalReport)
				r.closeSessWatch() // terminal from the loop's perspective; reap the watcher
				return
			}
			if markerSeen {
				s.emit(name, "done", round, 0, dur, "",
					"marker %q detected in session transcript — run is over", j.Marker)
			} else {
				s.emit(name, "done", round, 0, dur, "",
					"final report %s complete — run is over (ADR-0020)", j.FinalReport)
			}
			r.closeSessWatch() // terminal: reap the watcher (a run-log-only marker may never have pinned a transcript)
			return
		case !reviewing && j.Marker != "" && j.FinalReport != "" && s.markerAnywhere(r, markerSeen):
			// The agent signaled completion on SOME surface — assistant text,
			// a thinking block, a toolCall argument, the report file itself —
			// but the report never appeared. Ask for it, twice at most
			// (ADR-0020): a third ask just burns rounds, and the fatal names
			// the missing artifact so the remedy is a copy-paste.
			r.mu.Lock()
			asked := r.state.ReportSteers
			r.mu.Unlock()
			if asked >= reportSteerMax {
				msg := fmt.Sprintf("final report %s still missing after %d report requests — the agent keeps signaling completion without writing it", j.FinalReport, asked)
				r.finish("fatal", msg)
				s.logf(name, "FATAL: %s", msg)
				s.emit(name, "fatal", round, 0, 0, "", "%s", msg)
				return
			}
			r.mu.Lock()
			r.pendingReportSteer = true
			r.mu.Unlock()
			s.logf(name, "round %d: completion signal without report %s — report request #%d armed", round, j.FinalReport, asked+1)
			s.emit(name, "report_requested", round, 0, 0, "",
				"completion marker seen but final report %s missing — steering the agent to write it (request #%d; fatal after %d unanswered)",
				j.FinalReport, asked+1, reportSteerMax)
		}

		// A CI-stall cap intervention may have closed the run mid-round
		// (ADR-0004). The done-check above already had its chance to upgrade
		// a finished report to done; otherwise the fatal stands.
		if r.stateSnapshot().State == "fatal" {
			s.logf(name, "round %d: run closed by CI-stall intervention", round)
			r.closeSessWatch()
			return
		}

		// Review campaign gate (ADR-0012 §4.2): the campaign's exit condition
		// is "0 open threads AND CI pass", checked between rounds. It replaces
		// the old trailing `pre-merge --pr N` — the daemon holds that state, so
		// re-deriving it on demand was redundant. Runs ONLY in the reviewing
		// phase, so a build job never takes this path.
		if reviewing && s.reviewGate(r, round) {
			return
		}

		if instant && strikes >= 3 {
			r.finish("fatal", "3 consecutive instant exits — likely context exhaustion or model refusal")
			s.logf(name, "FATAL: 3 consecutive instant exits — manual intervention required")
			s.emit(name, "fatal", round, rc, dur, "", "3 consecutive instant exits — likely context exhaustion or model refusal")
			return
		}

		// First LAUNCH round must capture the session path (never re-LAUNCH:
		// a second launch forks the session).
		r.mu.Lock()
		pinned := r.job.SessionPath != ""
		r.mu.Unlock()
		if !pinned {
			p := s.captureSession(r)
			if p == "" {
				r.finish("fatal", "could not capture session path after LAUNCH — refusing to fork a new session")
				s.logf(name, "FATAL: could not capture session path after LAUNCH — aborting instead of forking")
				s.emit(name, "fatal", round, 0, dur, "", "could not capture session path — refusing to fork")
				return
			}
			r.mu.Lock()
			r.job.SessionPath = p
			adopted := r.job
			r.mu.Unlock()
			_ = job.Save(adopted)
			s.logf(name, "round %d: captured session path %s", round, p)
		}

		// Adaptive backoff: a clean cap means pi is healthy and progressing,
		// an instant implosion means something is wrong and retrying fast just
		// burns the context wall. Scale lets tests and operators tune it.
		var sleep time.Duration
		switch {
		case rc == 0:
			sleep = 15 * time.Second
		case dur >= 1700:
			sleep = 20 * time.Second
		case instant:
			sleep = 90 * time.Second
		default:
			sleep = 30 * time.Second
		}
		r.mu.Lock()
		scale := r.job.BackoffScale
		r.mu.Unlock()
		if scale <= 0 {
			scale = 1.0
		}
		sleep = time.Duration(float64(sleep) * scale)
		s.emit(name, "backoff", round, rc, dur, "", "sleeping %s before round %d", sleep.Round(time.Second), round+1)
		select {
		case <-stopCh:
			r.mu.Lock()
			src := r.stopSource // "operator" or "daemon" (ADR-0017); set before close
			r.state.State, r.state.StopSource, r.active = "stopped", src, false
			r.mu.Unlock()
			r.persistState()
			r.closeSessWatch()
			info := "operator stop during backoff"
			if src == "daemon" {
				info = "daemon shutdown during backoff — a re-arming watcher will resume this job"
			}
			s.emit(name, "stopped", round, rc, dur, "", "%s", info)
			return
		case <-time.After(sleep):
		}
	}
}

// markerSeenNow is the ONE funnel for the completion gate's marker surfaces:
// the sticky transcript latch, the cumulative transcript scan, and the current
// round's run log. The cap switch and the gate both call this, so they can
// never disagree about what "seen" means — the pre-funnel copies were exactly
// the drift the cap switch's own comment warned against ("Use the SAME marker
// surfaces the completion gate uses"), re-implemented instead of shared.
func (s *Supervisor) markerSeenNow(r *runner) bool {
	r.mu.Lock()
	latched := r.state.MarkerSeen
	marker := r.job.Marker
	sess := r.job.SessionPath
	name := r.job.Name
	r.mu.Unlock()
	if latched {
		return true
	}
	if marker == "" {
		return false
	}
	if sess != "" && job.TranscriptContains(sess, marker) {
		return true
	}
	return job.RunlogContains(job.Runlog(name), marker)
}

// markerAnywhere (ADR-0020) reports whether the AGENT produced the marker on
// a surface it authors — assistant text (carried in via markerSeen, already
// computed by the gate), a thinking block, a toolCall argument — or wrote it
// as the last line of a report written during THIS run. It is a
// completion-INTENT signal, never a completion signal: it may trigger a
// report request, never a done (invariant 15 — only assistant text counts).
//
// Deliberately NOT intent (qodo PR#7 finding 3): user-message quotes — every
// brief tells the agent which marker to emit and pi records the brief as a
// user message, so counting them armed two premature report requests on a
// healthy multi-round job and fatality'd it by round 3; compaction summaries
// (they quote history verbatim); and toolResult quotes (command OUTPUT — a
// grep of the brief, another agent's text — not the agent signaling).
//
// markerSeen is passed in because the gate already computed the sticky latch
// plus transcript scan this boundary; calling markerSeenNow here again would
// re-scan the transcript on every round (kody, PR#7 — the ask path is the
// only caller now, so the done path pays zero extra scans).
func (s *Supervisor) markerAnywhere(r *runner, markerSeen bool) bool {
	if markerSeen {
		return true
	}
	r.mu.Lock()
	marker := r.job.Marker
	report := r.job.FinalReport
	sess := r.job.SessionPath
	startedAt := r.state.StartedAt
	r.mu.Unlock()
	if marker == "" {
		return false
	}
	// A report ending with the marker is intent — but only a report written
	// during THIS run (qodo PR#7 finding 4: a stale report left by a previous
	// run must not re-arm asks on a fresh restart; Restart --fresh quarantines
	// it, and the mtime floor excludes the rest).
	if report != "" && reportReady(report, startedAt) && job.ReportEndsWithMarker(report, marker) {
		return true
	}
	if sess == "" {
		return false
	}
	srf := job.ScanMarkerSurfaces(sess, marker)
	return srf.Thinking > 0 || srf.ToolArgs > 0
}

// reportReady (ADR-0020) reports whether the final report exists and was
// written during THIS run. The mtime floor keeps a report left behind by a
// previous run from closing a fresh restart done before a single round runs;
// Restart clears StartedAt so the floor moves with the new run, and
// restart --fresh additionally quarantines the old report outright.
//
// The floor carries ONE minute of slack on purpose: the sanctioned operator
// recovery for a fatal-with-finished-work is to re-derive the gates, write
// the report, then Start — the report lands seconds BEFORE the new StartedAt
// and must still count (mealime-scroll, 2026-10-08). A stale report cannot
// ride that slack, because Restart --fresh moves it out of the way first.
func reportReady(path, startedAtRFC string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if startedAtRFC == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, startedAtRFC)
	if err != nil {
		return true
	}
	return !fi.ModTime().Before(t.Add(-time.Minute))
}

// markerSurfaceDiag names where the marker lives when no assistant text block
// emitted it. position-restore (2026-10-08, PR #203) ended fatal with "round
// cap 12 reached without marker" while the agent had written the marker into
// 3 thinking blocks, a toolCall argument, and the final report file — the
// message sent the operator hunting a marker that was never missing, only in
// the wrong place. "" when the marker is genuinely nowhere.
func (s *Supervisor) markerSurfaceDiag(r *runner) string {
	r.mu.Lock()
	marker := r.job.Marker
	report := r.job.FinalReport
	sess := r.job.SessionPath
	r.mu.Unlock()
	if marker == "" {
		return ""
	}
	var parts []string
	if job.ReportEndsWithMarker(report, marker) {
		parts = append(parts, "the final report FILE ends with it")
	}
	if sess != "" {
		if srf := job.ScanMarkerSurfaces(sess, marker); !srf.Empty() {
			if srf.Thinking > 0 {
				parts = append(parts, fmt.Sprintf("%d assistant thinking block(s)", srf.Thinking))
			}
			if srf.ToolArgs > 0 {
				parts = append(parts, fmt.Sprintf("%d toolCall argument(s)", srf.ToolArgs))
			}
			if srf.ToolResult > 0 {
				parts = append(parts, fmt.Sprintf("%d toolResult quote(s)", srf.ToolResult))
			}
			if srf.User > 0 {
				parts = append(parts, fmt.Sprintf("%d user-message quote(s)", srf.User))
			}
			if srf.Compaction > 0 {
				parts = append(parts, fmt.Sprintf("%d compaction summary quote(s)", srf.Compaction))
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " — but the marker WAS written: " + strings.Join(parts, ", ") +
		"; only an assistant TEXT block counts (ADR-0011/ADR-0020): steer the agent to end a message with the marker line"
}

// deliverPendingReportSteer delivers the ADR-0020 report request into a live
// round: once the client has published its pid, an abort+prompt pair asks the
// agent to write the missing final report. Round-scoped like the other
// watchers; a no-op when nothing is pending. The pending flag is cleared only
// on a successful write, so a failed delivery retries next round without
// double-counting (state.ReportSteers increments on actual delivery).
func (s *Supervisor) deliverPendingReportSteer(r *runner, round int, watchStop, stopCh chan struct{}) {
	r.mu.Lock()
	pending := r.pendingReportSteer
	report := r.job.FinalReport
	marker := r.job.Marker
	name := r.job.Name
	r.mu.Unlock()
	if !pending || marker == "" || report == "" {
		return
	}
	deadline := time.Now().Add(90 * time.Second)
	pid := 0
	for {
		r.mu.Lock()
		pid = r.pid
		r.mu.Unlock()
		if pid > 0 {
			break
		}
		select {
		case <-watchStop:
			return
		case <-stopCh:
			return
		default:
		}
		if time.Now().After(deadline) {
			// The round never went live; leave pending armed so the next
			// round delivers it. No ask was counted.
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	steer := "Your work looks complete (the completion marker was seen), but the final report is missing. " +
		"Write the final report NOW to " + report + ": what landed, files changed, gate/test results, PR URL. " +
		"Then end your assistant message with the marker line " + marker + "."
	// Invariant 8 (ADR-0005) applies to our own ask too: never count a
	// delivery we cannot prove. The client acks every control frame it acts
	// on; a prompt whose ack never turns terminal — client died after the
	// pid sample, round teardown truncating the ctrl file unread — was NOT
	// delivered and must not consume ask budget (qodo PR#7 finding 7). The
	// pending flag stays armed and the next round retries from a fresh
	// offset. Held is not terminal: the abort drain re-acks forwarded later.
	//
	// The offset is taken HERE — after the pid wait, therefore after
	// round() truncated the ack log for this round (this goroutine starts
	// BEFORE round() runs: snapshotting at spawn raced the truncation and
	// left the offset pointing past every record of the round), and before
	// our frames exist, so no ack of ours can precede it. The id filter
	// makes interleaved steers harmless.
	ackPath := job.Ack(name)
	off := job.Size(ackPath)
	id, err := s.interruptWith(name, steer)
	if err != nil {
		s.logf(name, "ERROR: report-request steer not delivered: %v", err)
		return // still pending; next round retries
	}
	deadline = time.Now().Add(45 * time.Second)
	// check classifies the ack records seen so far. Returns true when this
	// goroutine's work is done (counted, rejected, or fatally unresolved);
	// false means keep polling. A held record NEVER short-circuits the scan:
	// the client acks held first and forwarded LATER in the same file, so
	// returning at the held record would loop forever on an already-delivered
	// prompt.
	check := func() bool {
		for _, a := range acksSince(ackPath, off) {
			if a.ID != id {
				continue
			}
			if !a.Terminal() {
				continue // held behind an abort drain; the forwarded record follows
			}
			if a.Outcome != job.AckForwarded {
				s.logf(name, "report-request steer rejected by the client (%s); retries next round", a.Outcome)
				return true // still pending, no ask counted
			}
			r.mu.Lock()
			r.pendingReportSteer = false
			asked := r.state.ReportSteers + 1
			r.state.ReportSteers = asked
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "round %d: report-request steer delivered (ask %d/%d)", round, asked, reportSteerMax)
			return true
		}
		return false
	}
	for {
		if check() {
			return
		}
		select {
		case <-watchStop:
			// Round teardown takes one LAST look: the ack can land in the
			// final instants of the round (observed: the prompt forwarded
			// ~240ms before round end — the abort drains only when the
			// sleeping turn returns). Without this the delivery silently
			// exits unconfirmed and the ask re-arms next round.
			check()
			return
		case <-stopCh:
			return
		default:
		}
		if time.Now().After(deadline) {
			// Unconfirmed: leave pending armed, count nothing. The frame
			// may yet be read this round (the ctrl file is not truncated
			// until the NEXT round starts), but without an ack we cannot
			// claim it — and if it is read, the re-arm at the next boundary
			// is a no-op re-send of the same request, not a lost one.
			s.logf(name, "report-request steer unconfirmed within 45s; retries next round, no ask counted")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stateSnapshot copies the runner's persisted state under its lock.
func (r *runner) stateSnapshot() job.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// taskProgress counts the job's CURRENT plugin-task list through the ONE
// store adapter (owner amendment). Identity comes from the pinned session
// file (pi names transcripts <timestamp>_<sessionID>.jsonl) and the worktree
// (the child's cwd). No session identity → fallback with reason; memory/off
// or unreadable store → fallback 0/0 PLUS reason+explanation. I/O happens
// HERE, outside every supervisor lock; callers pass copied paths.
func taskProgress(sess, worktree string) *job.TaskProgress {
	sid := ""
	if sess != "" {
		sid = sessionIDFromPath(sess)
	}
	if sid == "" {
		return &job.TaskProgress{Reason: string(fault.KindTaskNoIdentity), Detail: taskReasonDetail(fault.KindTaskNoIdentity)}
	}
	res := taskwatch.Resolver{SessionID: sid, Cwd: worktree}
	tgt := res.Resolve()
	completed, total, why := res.Counts()
	if why == "" {
		// Valid counts (an EMPTY list is valid and error-free).
		path := tgt.Path
		if tgt.Memory {
			path = ""
		}
		return &job.TaskProgress{Completed: completed, Total: total, StorePath: path, Valid: true}
	}
	path := tgt.Path
	if tgt.Unavailable {
		path = ""
	}
	var kind fault.Kind
	switch why {
	case taskwatch.ReasonMemoryStore:
		kind = fault.KindTaskStoreMemory
	case taskwatch.ReasonInvalidData:
		kind = fault.KindTaskStoreInvalid
	default:
		kind = fault.KindTaskStoreMissing
	}
	return &job.TaskProgress{
		Completed: completed, Total: total,
		Reason: string(kind), Detail: taskReasonDetail(kind),
		StorePath: path,
	}
}

// taskReasonDetail is the human half of a structured task-status failure.
func taskReasonDetail(kind fault.Kind) string {
	switch kind {
	case fault.KindTaskNoIdentity:
		return "no session identity yet (the job has not pinned a pi session), so the plugin task store cannot be resolved"
	case fault.KindTaskStoreMemory:
		return "the plugin runs memory-only (PI_TASKS=off or taskScope=memory): there is no readable task store"
	case fault.KindTaskStoreInvalid:
		return "the task store exists but is unreadable or malformed"
	case fault.KindTaskStoreMissing:
		return "the task store for this session is missing (the plugin has not persisted one yet)"
	default:
		return string(kind)
	}
}

// live reports whether a round is currently polling the control file.
// sessionResolver is the fresh-LAUNCH transcript source: internal/job's
// fsnotify SessionWatcher. An interface, not the concrete type, so the wiring
// test can observe the supervisor ARM and CONSULT the watcher — the watcher
// was once shipped fully unit-tested and never called, and only an assertion
// about the wiring (not the component) catches that.
type sessionResolver interface {
	TryPath() string
	Wait(timeout time.Duration) string
	Close() error
}

// newSessionResolver arms the watcher for a LAUNCH round. Package variable so
// supervisor_test can wrap the real constructor and count its uses; production
// code never reassigns it.
var newSessionResolver = func(worktree string, notBefore time.Time) (sessionResolver, error) {
	return job.NewSessionWatcher(worktree, notBefore)
}

func (r *runner) live() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active && r.pid > 0
}

func (r *runner) finish(state, diag string) {
	r.mu.Lock()
	r.state.State, r.state.LastDiag, r.active = state, diag, false
	// A terminal state spends the campaign (ADR-0016): a later start must not
	// re-enter `reviewing` with a stale budget. Operator stops do NOT go
	// through finish (Stop writes its state directly), so a stopped campaign
	// stays armed and resumes on the next start.
	r.review = nil
	name := r.job.Name
	r.mu.Unlock()
	r.persistState()
	_ = name
	r.closeSessWatch() // a job that never adopted a transcript must not leak its watcher
}

// campaignReplyContract is the MANDATORY part of every campaign round prompt
// (owner directive 2026-10-08). The operator brief carries context and triage
// guidance; this block carries the non-negotiable obligation: a round answers
// its open threads with post_replies, through the shim, before it ends. The
// flambette#65 campaign showed every way a round can end WITHOUT replying —
// empty provider responses, build-brief drift, a mid-round kill — and a round
// that never reached post_replies is the one outcome the budget buys nothing
// for.
const campaignReplyContract = "<!-- daemon-mandatory: the supervisor injected this block; do not skip -->\n" +
	"## MANDATORY: reply to every open thread this round\n\n" +
	"Your round is NOT complete until `_pi-supervisor-review post_replies` has\n" +
	"answered **EVERY open thread** on the PR — exactly once each, this round:\n\n" +
	"- **Real defect** → fix it in code (build must stay green), then reply with\n" +
	"  the fix description and the commit.\n" +
	"- **Non-issue** → reply with the evidence (file:line, gate/ADR reference).\n" +
	"  Findings contradicting a recorded ADR decision are non-issues: rebut with\n" +
	"  the ADR line, do not renegotiate.\n" +
	"- **Out of scope** → reply stating the deferral explicitly.\n\n" +
	"Start with `list_threads`, triage, do the work, then `post_replies` with\n" +
	"the batch, then `resolve_thread` per answered thread (reply BEFORE resolve —\n" +
	"the daemon refuses a resolve with no reply this round). **Ending the turn\n" +
	"without a post_replies call is a failed round**: the supervisor counts the\n" +
	"round against the budget and the threads stay open.\n\n" +
	"If you are genuinely blocked (auth, missing data), say so in your final\n" +
	"message AND reply to the threads you could answer anyway — partial beats\n" +
	"silent.\n"

// writeCampaignRoundPrompt regenerates the campaign round prompt: the
// operator's review brief wrapped by the daemon's mandatory-reply contract.
// Regenerated EVERY round so a brief edit mid-campaign lands on the next
// round; the file lives at the canonical per-job path.
func (s *Supervisor) writeCampaignRoundPrompt(name, briefPath string) (string, error) {
	body, err := os.ReadFile(briefPath)
	if err != nil {
		return "", fmt.Errorf("read review brief: %w", err)
	}
	var b strings.Builder
	b.WriteString(campaignReplyContract)
	b.WriteString("\n---\n\n")
	b.Write(body)
	// Private per-job directory (kody PR#9 re-review): a fixed predictable
	// path under the shared /tmp is either a symlink-truncation hazard
	// (O_NOFOLLOW) or — once the open fails loudly — a one-time plant that
	// denies the campaign forever. A 0700 directory with a random suffix
	// removes both: nothing predictable can be pre-planted, and the write
	// is owner-only. The directory persists across rounds (the prompt is
	// regenerated into it every round, and round N+1 overwrites the same
	// file), so no temp litter accumulates.
	dir := "/tmp/pi_" + name + "_review"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create review prompt dir: %w", err)
	}
	p := filepath.Join(dir, "round_prompt.md")
	// O_NOFOLLOW stays: defense in depth if anything did plant a symlink
	// INSIDE the 0700 dir (requires the daemon's own uid, i.e. a compromised
	// process — not an external attacker).
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("create round prompt: %w", err)
	}
	if _, err := f.Write([]byte(b.String())); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write round prompt: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close round prompt: %w", err)
	}
	return p, nil
}

// round runs one pi RPC round via the daemon-native client (internal/client);
// hard-kills the pi process group at timeout+120s as a backstop.
func (s *Supervisor) round(r *runner, round int, stopCh chan struct{}) (rc int, dur int64, runlogB int64) {
	r.mu.Lock()
	j := r.job
	resume := j.SessionPath != ""
	reviewing := r.state.State == "reviewing"
	r.mu.Unlock()

	// Stamp the launch time BEFORE spawning. Session discovery (below, and in
	// the marker watcher) must only ever adopt a transcript created at or after
	// this instant: pi writes its JSONL asynchronously, so a scan issued right
	// after the spawn can beat the file into existence and otherwise return
	// some older run's transcript. Pinning that stale path breaks the round in
	// a way that looks like a hung agent — no transcript growth, no completion
	// marker (ADR-0011), and the empty-turn detector aborts a healthy session.
	// Only meaningful for a LAUNCH; a RESUME already has its path pinned.
	if !resume {
		r.mu.Lock()
		r.launchedAt = time.Now()
		floor := r.launchedAt
		r.mu.Unlock()
		// Arm the fsnotify session watcher BEFORE the spawn: pi writes its
		// JSONL asynchronously, so a directory scan issued right after the
		// spawn can beat the file into existence (the stale-adoption bug),
		// and polling for the file leaves the marker gate blind for a whole
		// poll interval after the transcript lands. The watcher adopts the
		// file the moment the kernel reports it. A failed arm is not fatal:
		// captureSession falls back to the FindSession poll, which is exactly
		// the pre-watcher behavior.
		if w, err := newSessionResolver(j.Worktree, floor); err != nil {
			s.logf(j.Name, "round %d: session watcher unavailable (%v); falling back to FindSession polling", round, err)
		} else {
			r.mu.Lock()
			r.sessWatch = w
			startWait := w
			r.mu.Unlock()
			// Adopt from the EVENT side, not only from the scan: a goroutine
			// parks in Wait until the kernel reports the transcript and pins
			// it at once, so no tick boundary sits between the file landing
			// and its adoption (TryPath alone would leave the inotify watch
			// opened and unused, making the watcher equivalent to the poll it
			// replaced). Close unblocks Wait — its events channel closes and
			// it answers from its final scan — so this goroutine always exits
			// once the watcher is reaped.
			go func() {
				p := startWait.Wait(0)
				if p == "" {
					return
				}
				r.mu.Lock()
				// Gate on `active` as well as the empty path. Restart
				// (ADR-0010) clears SessionPath and a concurrent Stop
				// closes the watcher that unblocks Wait, so this
				// goroutine can wake AFTER that reset and observe the
				// cleared field. Pinning then would make the next
				// round compute resume=true and RESUME the session
				// `restart --fresh` just quarantined — resurrecting the
				// exact stale-adoption bug the notBefore floor fixed.
				// Only a still-live round may adopt.
				fresh := r.active && r.job.SessionPath == ""
				var adopted job.Job
				if fresh {
					r.job.SessionPath = p
					adopted = r.job
				}
				r.mu.Unlock()
				if fresh {
					_ = job.Save(adopted)
					s.logf(j.Name, "round %d: adopted session transcript %s (fsnotify)", round, p)
				}
			}()
		} // Reaped by closeSessWatch once the path is pinned or the job turns
		// terminal — the adoption sites run AFTER round() returns, so the
		// watcher must outlive the round's client.
	}

	_, runlog, _, _ := job.Paths(j.Name)
	_ = os.Truncate(runlog, 0)
	// Fresh steer channel per round: the client reads the ctrl file from its
	// start, so a leftover frame from the previous round would replay into
	// this one. The ack log is truncated with it (ADR-0005).
	if err := os.WriteFile(job.Ctrl(j.Name), nil, 0o644); err != nil {
		s.logf(j.Name, "round %d: cannot reset steer channel %s: %v", round, job.Ctrl(j.Name), err)
		return 1, 0, 0
	}
	if err := os.WriteFile(job.Ack(j.Name), nil, 0o644); err != nil {
		s.logf(j.Name, "round %d: cannot reset ack log %s: %v", round, job.Ack(j.Name), err)
	}

	promptPath := j.Brief
	if reviewing && j.ReviewBrief != "" {
		// ADR-0018: a campaign's rounds are seeded/RE-anchored with the
		// review brief — BOTH the round-1 LAUNCH and every later RESUME.
		// A resume that re-sent the BUILD continuation instead pulled the
		// agent back into build tasks mid-campaign (flambette#65 round 8:
		// "Now task 2 — identity store" while 12 threads sat open). The
		// brief restates the triage contract every round, which is exactly
		// the anchor a drifted session needs.
		//
		// The operator brief is WRAPPED with the daemon's mandatory-reply
		// contract (owner directive 2026-10-08): the brief is the operator's
		// context, but the REPLY OBLIGATION is the daemon's — it is the one
		// thing a round exists to produce. The wrapped file is regenerated
		// every round from the current brief, so a brief edit mid-campaign
		// takes effect on the next round (the skill's stop-campaign-first
		// rule stays: a reversal must never be picked up from a stale
		// brief while the owner is rewriting it).
		wrapped, err := s.writeCampaignRoundPrompt(j.Name, j.ReviewBrief)
		if err != nil {
			s.logf(j.Name, "round %d: cannot build the campaign round prompt: %v", round, err)
			return 1, 0, 0
		}
		promptPath = wrapped
	} else if resume {
		promptPath = j.Cont
	}

	if resume {
		s.logf(j.Name, "round %d: RESUME %s (prompt %s)", round, j.SessionPath, promptPath)
	} else {
		s.logf(j.Name, "round %d: LAUNCH prompt=%s", round, promptPath)
	}

	out, err := os.OpenFile(runlog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 1, 0, 0
	}
	// The budget's "did this round produce anything" signal must describe
	// THIS round: lastText survives the backstop/stop return paths (which
	// never refresh it), so a stale value from the previous round would
	// otherwise spend budget on an empty one (kody PR#9 follow-up). Clear
	// at round start; only a completed round's result re-fills it.
	r.mu.Lock()
	r.lastText = ""
	r.mu.Unlock()
	// The run log is a diagnostic mirror; the round's verdict comes from
	// the client Result, so a failed close (append-only handle) is not fatal.
	defer func() { _ = out.Close() }()

	prompt, err := client.DefaultPromptFile(promptPath)
	if err != nil {
		s.logf(j.Name, "round %d: prompt unreadable: %v", round, err)
		return 1, 0, 0
	}

	// ADR-0014: wire the TaskUpdate completion watch into THIS round. The
	// watcher is round-scoped: one worker goroutine, buffered channels, and
	// a bounded flush before the round is classified, so the terminal close
	// can never precede a queued task event and stop/shutdown stay bounded.
	tw := newTaskWatcher(s, j.Name, j.SessionPath, round)
	go tw.run()
	defer tw.finish() // the flush happens BEFORE this round's rc is used
	opts := client.Options{
		PiBin:        j.PiBin,
		Session:      j.SessionPath,
		Name:         j.SessionName,
		Worktree:     j.Worktree,
		Skills:       j.Skills,
		Provider:     j.Provider,
		Model:        j.Model,
		Prompt:       prompt,
		ControlPath:  job.Ctrl(j.Name),
		AckPath:      job.Ack(j.Name),
		PollInterval: controlPollInterval,
		Timeout:      time.Duration(j.TimeoutS) * time.Second,
		GracePeriod:  30 * time.Second,
		Observations: tw.obsCh,
		Identity:     tw.identCh,
		Out:          out,
		Diag:         out,
		OnPID: func(pid int) {
			r.mu.Lock()
			r.pid = pid
			r.started = time.Now()
			r.mu.Unlock()
		},
	}

	type outcome struct {
		res client.Result
	}
	doneCh := make(chan outcome, 1)
	start := time.Now()
	go func() {
		doneCh <- outcome{res: client.Run(opts)}
	}()

	backstop := time.NewTimer(time.Duration(j.TimeoutS+120) * time.Second)
	defer backstop.Stop()

	clearPID := func() {
		r.mu.Lock()
		r.pid = 0
		r.mu.Unlock()
	}

	select {
	case o := <-doneCh:
		clearPID()
		r.mu.Lock()
		r.lastText = o.res.Text // the budget reads THIS, not the runlog
		r.mu.Unlock()
		if o.res.Err != "" {
			s.logf(j.Name, "round %d: client error: %s", round, o.res.Err)
		}
		return o.res.RC, int64(time.Since(start).Seconds()), job.Size(runlog)

	case <-backstop.C:
		// Last-resort net: the client owns its own soft deadline, so this only
		// fires if the client goroutine itself wedged.
		r.mu.Lock()
		pid := r.pid
		r.mu.Unlock()
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		s.logf(j.Name, "round %d: hard kill after %ds (supervisor backstop)", round, int(time.Since(start).Seconds()))
		o := <-doneCh
		clearPID()
		return o.res.RC, int64(time.Since(start).Seconds()), job.Size(runlog)

	case <-stopCh:
		s.logf(j.Name, "round %d: operator stop — terminating pi process group", round)
		// Escalate fast and deterministically so an operator stop (and test
		// teardown) never leaves an orphan pi writing to the session:
		// SIGTERM the group, then SIGKILL after a short grace. This mirrors
		// Stop() but also covers the case where Stop() fired before OnPID
		// published the pid (pid==0 there), which is exactly when an
		// unsupervised pi would otherwise survive.
		r.mu.Lock()
		spid := r.pid
		r.mu.Unlock()
		if spid > 0 {
			_ = syscall.Kill(-spid, syscall.SIGTERM)
		}
		done := make(chan struct{})
		go func() {
			o := <-doneCh
			clearPID()
			doneCh <- o
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			r.mu.Lock()
			pid := r.pid
			r.mu.Unlock()
			if pid > 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
			<-done
		}
		o := <-doneCh
		return o.res.RC, int64(time.Since(start).Seconds()), job.Size(runlog)
	}
}

// watchMarker streams the session transcript for one round and latches the
// completion marker as pi emits it (ADR-0011).
//
// It runs WHILE the round is live, so the marker is detected mid-turn instead
// of only at the next post-round classification — the run-log path could only
// check after the fact, and after the fact the run log had already been
// truncated by the next round.
//
// `sess` may be empty (fresh LAUNCH: pi creates the transcript as it runs), in
// which case the path is re-resolved on each tick until it appears.
//
// The latch lives in the watcher (sticky for the round). This function only
// records it on the runner so `status` is truthful mid-round and the gate can
// read it at the round boundary. It never interrupts pi: the agent still has
// to end its turn cleanly, and `done` additionally requires final_report.
// closeSessWatch reaps the round's session watcher. Idempotent: the watcher
// is closed exactly once even when several paths race to reap it (Session-
// Watcher.Close is mutex-guarded), and a nil slot is a no-op.
func (r *runner) closeSessWatch() {
	r.mu.Lock()
	w := r.sessWatch
	r.sessWatch = nil
	r.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

// captureSession resolves the session transcript for a fresh LAUNCH. The
// armed SessionWatcher answers first — fsnotify adopts the file the moment
// pi creates it, which is the poll-interval gap the polling path cannot
// remove — and the FindSession poll stays as the fallback for a watcher that
// failed to arm or missed its events (fsnotify can coalesce under load; the
// watcher's own Wait re-scans for the same reason). Both apply the same
// notBefore floor and defer to the same dirScan selection rule, so watched
// and polled adoption can never disagree about which file is newest.
// Returns "" when nothing is discoverable yet.
func (s *Supervisor) captureSession(r *runner) string {
	r.mu.Lock()
	if p := r.job.SessionPath; p != "" {
		r.mu.Unlock()
		r.closeSessWatch() // already pinned: the watcher's job is done
		return p
	}
	w := r.sessWatch
	name, wt := r.job.SessionName, r.job.Worktree
	floor := r.launchedAt
	r.mu.Unlock()
	if w != nil {
		if p := w.TryPath(); p != "" {
			r.closeSessWatch()
			return p
		}
	}
	if p := job.FindSession(name, wt, floor); p != "" {
		r.closeSessWatch() // the poll won the race: stop watching
		return p
	}
	return ""
}

func (s *Supervisor) watchMarker(r *runner, sess, marker string, round int, watchStop, stopCh chan struct{}) {
	if marker == "" {
		return // no marker configured: nothing to detect (and the gate refuses)
	}
	r.mu.Lock()
	name := r.job.Name
	r.mu.Unlock()

	var w *job.TranscriptWatcher
	// Create the watcher EAGERLY, before pi can emit anything, so the seed
	// offset is the file's size at round start and no marker can slip through
	// the gap between "round began" and "watcher exists". A fresh LAUNCH has no
	// transcript yet, so resolution retries on the tick — but it is resolved
	// from the moment the file appears, before the first token is written.
	if sess != "" {
		w = job.NewTranscriptWatcher(sess, marker, 0)
	}
	ensure := func() *job.TranscriptWatcher {
		if w != nil {
			return w
		}
		path := sess
		if path == "" {
			r.mu.Lock()
			path = r.job.SessionPath
			r.mu.Unlock()
		}
		if path == "" {
			path = s.captureSession(r)
			if path == "" {
				return nil
			}
			r.mu.Lock()
			r.job.SessionPath = path
			r.mu.Unlock()
			_ = job.Save(r.job)
		}
		w = job.NewTranscriptWatcher(path, marker, 0)
		return w
	}

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-watchStop:
			// Final fold so a marker written in the last 2s of the round is
			// not lost to the tick boundary. Latch-ONLY: a fresh watcher in
			// round N>1 starts its offset AFTER the historical marker, so
			// mw.Seen() is false for a marker latched in an earlier round —
			// writing that false here un-latched the sticky latch (exposed
			// by the ADR-0020 grace rounds, which run whole rounds after the
			// latch; TestCapDiagnosticNamesMissingReportNotMarker caught it
			// via marker_seen=false in the final state).
			if mw := ensure(); mw != nil {
				mw.Poll()
				if mw.Seen() {
					s.setMarkerSeen(r, true, round)
				}
			}
			return
		case <-stopCh:
			return
		case <-t.C:
			mw := ensure()
			if mw == nil {
				continue // transcript not created yet
			}
			if mw.Poll() {
				s.logf(name, "round %d: marker %q streamed from session transcript", round, marker)
				s.setMarkerSeen(r, true, round)
				return // latched; no further polling needed this round
			}
		}
	}
}

// setMarkerSeen records the completion latch on the runner.
func (s *Supervisor) setMarkerSeen(r *runner, seen bool, round int) {
	r.mu.Lock()
	already := r.state.MarkerSeen
	r.state.MarkerSeen = seen
	r.mu.Unlock()
	if seen && !already {
		s.logf(r.job.Name, "round %d: completion marker detected in transcript", round)
	}
	if seen {
		r.persistState()
	}
}

// watchCIStalls tails the session transcript for one round and intervenes at
// the stall cap (ADR-0004). Non-cap stalls only emit; the cap stall writes
// abort+prompt frames telling the agent to finish the report, then closes the
// run as a failure to finish the review loop. The round itself is left alive
// so the report turn can actually run.
func (s *Supervisor) watchCIStalls(r *runner, sess string, round int, watchStop, stopCh chan struct{}) {
	r.mu.Lock()
	capN, idleS, name := r.job.CIStallCap, r.job.CIStallIdleS, r.job.Name
	r.mu.Unlock()
	if capN <= 0 {
		capN = 3
	}
	if idleS <= 0 {
		idleS = 300
	}
	d := stall.New(sess, time.Duration(idleS)*time.Second)
	tick := min(max(time.Duration(idleS)/10, 200*time.Millisecond), 15*time.Second)
	t := time.NewTicker(tick)
	defer t.Stop()
	// Final scan on every exit path: the last lines of a round's transcript
	// (where the agent links its PR) are written as the client exits, so the
	// regular tick can miss them. Without this the PR URL would only surface
	// on the round AFTER the one that opened it.
	defer func() {
		d.Poll() // final verdict is irrelevant; only the PR scrape matters
		s.recordPR(r, d.PRURL())
	}()
	for {
		select {
		case <-watchStop:
			return
		case <-stopCh:
			return
		case <-t.C:
			stalled, marker := d.Poll()
			s.recordPR(r, d.PRURL())
			if !stalled {
				continue
			}
			r.mu.Lock()
			r.state.CIStalls++
			n := r.state.CIStalls
			r.mu.Unlock()
			s.logf(name, "round %d: CI/review stall %d/%d (%s)", round, n, capN, marker)
			s.emit(name, "ci_stall", round, 0, 0, "",
				"stall %d/%d — agent parked on CI/review: %s", n, capN, marker)
			if n < capN {
				continue
			}
			// Cap reached: instruct the agent to finish the report, then
			// close the run as a review-loop failure. Keep the round alive
			// so the abort+prompt turn can actually deliver the report.
			interrupt := "CI/review stall cap reached (" + fmt.Sprint(capN) +
				" parks with no transcript progress). Stop polling CI. Finish the review " +
				"report NOW: summarize what landed, what failed, and what the operator " +
				"must check. The supervisor is closing this run as a failure to finish " +
				"the review loop."
			_, interruptErr := s.interruptWith(name, interrupt)
			if interruptErr != nil {
				// The round can no longer be told to finish the report, so the
				// agent will be killed without it — surface that instead of
				// silently reporting "agent instructed".
				s.logf(name, "ERROR: CI-stall cap interrupt not delivered: %v", interruptErr)
			}
			delivered := ""
			if interruptErr == nil {
				delivered = "; agent instructed to finish the report"
			} else {
				delivered = "; FINISH-THE-REPORT INTERRUPT NOT DELIVERED (" +
					interruptErr.Error() + ")"
			}
			r.mu.Lock()
			r.state.State, r.state.LastDiag, r.active = "fatal",
				"CI review retry cap exceeded — review loop did not finish"+delivered, false
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "FATAL: CI review retry cap %d exceeded%s", capN, delivered)
			s.emit(name, "fatal", round, 0, 0, "",
				"CI review retry cap %d exceeded — review loop did not finish%s", capN, delivered)
			return
		}
	}
}

// watchEmptyTurn detects the "alive but producing nothing" round that
// otherwise burns the full timeout_s with no error signal (ADR-0010): the
// transcript stays frozen and no tool call is seen for the whole window. It
// emits an empty_turn event on the first detection and then escalates on the
// same abort+prompt path as the CI-stall cap, so a wedged turn is interrupted
// and re-prompted instead of waited out.
func (s *Supervisor) watchEmptyTurn(r *runner, sess string, round int, watchStop, stopCh chan struct{}) {
	r.mu.Lock()
	idleS, name := r.job.EmptyTurnIdleS, r.job.Name
	r.mu.Unlock()
	if idleS <= 0 {
		idleS = 60
	}
	d := stall.New(sess, time.Duration(idleS)*time.Second)
	tick := min(max(time.Duration(idleS)*time.Second/5, time.Second), 15*time.Second)
	t := time.NewTicker(tick)
	defer t.Stop()
	reported := false
	for {
		select {
		case <-watchStop:
			return
		case <-stopCh:
			return
		case <-t.C:
			d.Poll() // keep the offset/PR/tool bookkeeping fresh
			stalled, quiet := d.EmptyTurn(stall.EmptyTurnWindow{
				Idle: time.Duration(idleS) * time.Second, MinGrowth: 1,
			})
			if !stalled {
				continue
			}
			if !reported {
				reported = true
				s.logf(name, "round %d: empty-turn stall — transcript frozen %s, 0 tool calls", round, quiet.Round(time.Second))
				s.emit(name, "empty_turn", round, 0, 0, "",
					"empty turn: no transcript growth and no tool call for %s", quiet.Round(time.Second))
				continue
			}
			// Second consecutive empty window: the round is wedged, not slow.
			// Same escalation as the CI-stall cap: abort + re-prompt, and tell
			// the round's caller why.
			msg := "empty-turn stall detected twice (" +
				quiet.Round(time.Second).String() + " of no transcript growth and no tool call). " +
				"Stop waiting and act: summarize the current state, commit whatever is complete, " +
				"and write the final report. If you are blocked, say exactly what you are blocked on."
			_, err := s.interruptWith(name, msg)
			delivered := "; agent re-prompted"
			if err != nil {
				delivered = "; RE-PROMPT NOT DELIVERED (" + err.Error() + ")"
			}
			r.mu.Lock()
			r.state.LastDiag = "empty-turn stall — no progress" + delivered
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "round %d: empty-turn stall escalated%s", round, delivered)
			s.emit(name, "empty_turn_escalated", round, 0, 0, "",
				"empty-turn stall: round produced nothing twice in a row%s", delivered)
			return
		}
	}
}

// recordPR stores the first pull-request URL seen in the round's transcript
// on the runner (ADR-0006). Best-effort and eventually consistent: an empty
// url means "not linked", never "no PR exists". Lock order stays s.mu -> r.mu:
// this is called from the watcher goroutine with neither held.
func (s *Supervisor) recordPR(r *runner, url string) {
	if url == "" {
		return
	}
	r.mu.Lock()
	fresh := r.state.PRURL != url
	if fresh {
		r.state.PRURL = url
	}
	name := r.job.Name
	r.mu.Unlock()
	if fresh {
		s.logf(name, "pull request: %s", url)
	}
}

// interruptWith delivers an instant abort+fresh-prompt pair to a live round's
// control file — the same wire format pi_control.py --interrupt writes and
// the client's abort-drain handshake consumes — and returns the PROMPT
// frame's id, so the caller can wait for the client's ack record (ADR-0005)
// and prove the delivery before counting it. The write is only a request;
// the ack is the receipt.
func (s *Supervisor) interruptWith(name, text string) (string, error) {
	f, err := os.OpenFile(job.Ctrl(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close() // control file: the frames are already written below
	rid := fmt.Sprintf("%d", time.Now().UnixNano())
	promptID := "int-" + rid
	for _, frame := range []any{
		map[string]any{"id": "abort-" + rid, "type": "abort"},
		map[string]any{"id": promptID, "type": "prompt", "message": text},
	} {
		line, _ := json.Marshal(frame)
		if _, err := f.Write(append(line, '\n')); err != nil {
			return "", err
		}
	}
	return promptID, nil
}

// emit records one lifecycle event (audit JSONL + live fan-out to watches).
// info is printf-formatted for readable diagnostics. The job's worktree is
// attached so a waking LLM can target verification at the right path.
// emit is the single funnel every lifecycle transition already flows through,
// so the journal line is emitted here rather than at ~40 call sites (DRY):
// one call means the structured log cannot drift from the event stream.
func (s *Supervisor) emit(jobName, event string, round, rc int, durS int64, text, format string, args ...any) {
	if len(text) > 200 {
		text = text[len(text)-200:]
	}
	s.emitEnvelope(jobName, &events.Event{
		Event: event, Round: round, RC: rc, DurS: durS, Text: text,
	}, format, args...)
}

// emitEnvelope is the funnel's shared body: fill the job context, write ONE
// journal line, publish ONE event. Both lifecycle emits and task-watch emits
// (ADR-0014) reach the audit/broker through exactly this path, so the log
// cannot drift from the stream no matter who calls.
//
// The journal line carries identifiers/outcome only — callers must not put
// task descriptions or payloads into info.
func (s *Supervisor) emitEnvelope(jobName string, e *events.Event, format string, args ...any) {
	info := format
	if len(args) > 0 {
		info = fmt.Sprintf(format, args...)
	}
	var wt, sess, pr string
	s.mu.Lock()
	if r, ok := s.jobs[jobName]; ok {
		r.mu.Lock()
		wt = r.job.Worktree
		sess = r.job.SessionPath
		pr = r.state.PRURL
		r.mu.Unlock()
	}
	s.mu.Unlock()
	e.Job = jobName
	e.Info = info
	e.Worktree = wt
	e.SessionPath = sess
	e.PRURL = pr
	s.logEvent(jobName, e.Event, e.Round, e.RC, e.DurS, info)
	events.Emit(*e)
}

// logEvent writes one structured line for a lifecycle transition. Level is
// derived from the event: a failed or halted run must be visible in
// `journalctl -p warning` without a filter, ordinary progress must not be.
func (s *Supervisor) logEvent(jobName, event string, round, rc int, durS int64, info string) {
	l := journal.Subsys("job")
	args := []any{"job", jobName, "round", round, "rc", rc, "dur_s", durS, "detail", info}
	switch event {
	case "fatal", "review_exhausted", "review_gate_error":
		l.Error(logEventName(event), args...)
	case "stopped", "bulk_resolve_expired", "review_skipped":
		l.Warn(logEventName(event), args...)
	default:
		l.Info(logEventName(event), args...)
	}
}

// logRound writes a structured line for a non-transition round fact.
func (s *Supervisor) logRound(jobName, event string, round, rc int, durS int64, format string, args ...any) {
	journal.Subsys("job").Info(event, "job", jobName,
		"round", round, "rc", rc, "dur_s", durS, "detail", fmt.Sprintf(format, args...))
}

// logEventName renames an event-stream verb to its log counterpart so the two
// vocabularies stay recognizable without being identical.
func logEventName(event string) string {
	switch event {
	case "job_started":
		return "job_start"
	case "round_done":
		return "round_end"
	case "done":
		return "job_done"
	case "fatal":
		return "job_fatal"
	case "stopped":
		return "job_stopped"
	}
	return event
}

// Watch subscribes the caller to a job's lifecycle events ("" = all jobs).
// If the requested job is already terminal (done|fatal|stopped) it returns a
// non-nil precheck Event instead of a channel: the server sends it and closes
// rather than blocking on a finished run (ADR-0003, requirement 4).
func (s *Supervisor) Watch(jobName string) (<-chan events.Event, func(), *events.Event) {
	id, ch, cancel := events.Subscribe()
	if jobName != "" {
		s.mu.Lock()
		r, ok := s.jobs[jobName]
		s.mu.Unlock()
		if ok {
			snap := r.snapshot()
			switch snap.State {
			case "stopped":
				// Watch-driven resume (ADR-0017): a stop recorded as coming
				// from the DAEMON's own shutdown was never an intent to end
				// the campaign — the daemon was merely going away. A watcher
				// re-arming IS the proof someone still cares, so the daemon
				// resumes the pinned session now. An OPERATOR stop stays
				// stopped: someone explicitly halted it. Only one of two
				// racing watchers performs the resume; the loser sees
				// AlreadyRunning and proceeds as a normal running-watch.
				r.mu.Lock()
				stoppedByDaemon := r.state.StopSource == "daemon"
				r.mu.Unlock()
				if stoppedByDaemon {
					if err := s.Start(jobName); err != nil {
						// Not AlreadyRunning: a genuine refusal (unknown job,
						// done without campaign, ...). Report it as the
						// precheck instead of silently subscribing to a job
						// that will never run — same fail-loud contract the
						// old precheck had.
						if fault.KindOf(err) != fault.KindAlreadyRunning {
							ev := events.Event{
								TS: time.Now().UTC().Format(time.RFC3339), Job: jobName,
								Event: "fatal", Round: snap.Round,
								Info:     "resume refused: " + err.Error(),
								Worktree: r.job.Worktree, SessionPath: r.job.SessionPath,
							}
							cancel()
							return nil, func() {}, &ev
						}
					} else {
						s.logf(jobName, "watch-driven resume (daemon-shutdown stop): resuming pinned session")
						s.emit(jobName, "job_started", snap.Round, 0, 0, "",
							"resumed by a re-arming watcher after daemon shutdown (ADR-0017)")
					}
					// Either way: the job is now running (or was already) —
					// fall through to the normal running-watch path below.
					break
				}
				fallthrough
			case "done", "fatal":
				r.mu.Lock()
				// Fields read under the same lock as the baseline check below.
				ev := events.Event{
					TS: time.Now().UTC().Format(time.RFC3339), Job: jobName,
					Event: snap.State, Round: snap.Round, RC: snap.LastRC,
					DurS:        snap.LastDurS,
					Info:        "run already " + snap.State + " — nothing to wait for",
					Worktree:    r.job.Worktree,
					SessionPath: r.job.SessionPath,
					PRURL:       r.state.PRURL,
				}
				hasBaseline := r.state.ReviewBaseline != nil
				r.mu.Unlock()
				// A finished job is NOT necessarily silent. If it ran a review
				// campaign, a later push can still attract findings that nothing
				// will answer (ADR-0012 follow-up), and this operator is exactly
				// who wants to hear about it. So answer immediately with the
				// precheck — as before — but STAY SUBSCRIBED instead of
				// unsubscribing, so review_threads_appeared can still arrive.
				//
				// Without a baseline there is nothing that can ever be emitted
				// for this job, so the old immediate return stands.
				if !hasBaseline {
					cancel()
					return nil, func() {}, &ev
				}
				ev.Info += " — but this job has a review baseline, so staying subscribed for late review findings (Ctrl-C to stop)"
				return ch, cancel, &ev
			}
		}
	}
	_ = id
	return ch, cancel, nil
}

// RunningCount reports how many jobs are currently in a running round. Fed
// to systemd as STATUS= on every watchdog beat.
// JobNames lists the loaded job names, sorted. Used by the daemon's own
// startup log line.
func (s *Supervisor) JobNames() []string {
	s.mu.Lock()
	names := make([]string, 0, len(s.jobs))
	for n := range s.jobs {
		names = append(names, n)
	}
	s.mu.Unlock()
	sort.Strings(names)
	return names
}

func (s *Supervisor) RunningCount() int {
	n, _ := s.StatusLine()
	return n
}

// StatusLine is RunningCount plus a note for `systemctl status`: the names of
// jobs whose state is fatal. "0 parallel pi session(s) running" on its own
// says nothing about WHY the daemon is idle, and a fatal job is exactly the
// thing an operator must notice without opening the logs.
func (s *Supervisor) StatusLine() (int, string) {
	s.mu.Lock()
	runners := make([]*runner, 0, len(s.jobs))
	for _, r := range s.jobs {
		runners = append(runners, r)
	}
	s.mu.Unlock()
	n := 0
	var fatal []string
	for _, r := range runners {
		r.mu.Lock()
		if r.active && r.state.State == "running" {
			n++
		}
		if r.state.State == "fatal" {
			fatal = append(fatal, r.job.Name)
		}
		r.mu.Unlock()
	}
	note := ""
	if len(fatal) > 0 {
		sort.Strings(fatal)
		note = "FATAL: " + strings.Join(fatal, ",")
	}
	return n, note
}

// statusList is the typed StatusAll used internally by Monitor.
func (s *Supervisor) statusList() []job.Status {
	s.mu.Lock()
	names := make([]string, 0, len(s.jobs))
	byName := make(map[string]*runner, len(s.jobs))
	for n, r := range s.jobs {
		names = append(names, n)
		byName[n] = r
	}
	s.mu.Unlock()
	sort.Strings(names)
	out := make([]job.Status, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n].snapshot())
	}
	return out
}

// Monitor refreshes per-job status JSON files every 2s.
func (s *Supervisor) Monitor(stop chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			for _, st := range s.statusList() {
				_, _, _, statusPath := job.Paths(st.Name)
				data, _ := json.MarshalIndent(st, "", "  ")
				_ = os.WriteFile(statusPath, data, 0o644)
			}
		}
	}
}

// Shutdown terminates all active round loops: it closes each runner's stop
// channel (so the loop's stop path runs and no NEW round is spawned) and
// SIGTERMs the live client groups, then waits for the loops to unwind.
func (s *Supervisor) Shutdown() {
	close(s.stop)
	s.mu.Lock()
	runners := make([]*runner, 0, len(s.jobs))
	for _, r := range s.jobs {
		runners = append(runners, r)
	}
	s.mu.Unlock()
	for _, r := range runners {
		r.mu.Lock()
		active, pid := r.active, r.pid
		stoppedAtRound := 0
		name := ""
		if active {
			// Same guarded close as Stop(): a concurrent operator Stop() must
			// not turn this into a double-close of the stop channel.
			r.active = false
			// Daemon shutdown (ADR-0017): recorded so a re-arming watcher can
			// RESUME this job — unlike an operator stop, nobody asked for the
			// campaign to end; the daemon is merely going away.
			r.stopSource = "daemon"
			close(r.stopCh)
			// Persist the terminal state HERE, exactly like Stop() does
			// (state-durability invariant). Shutdown races the loop
			// goroutine's first step: if the loop sees an already-closed
			// stop channel it returns silently without classifying, so this
			// write is the only durable record of the stop — without it the
			// state file kept saying "running" and the ADR-0017 resume
			// never armed (TestWatchResumesDaemonStoppedJob).
			if r.state.Round < 1 {
				r.state.Round = 1
			}
			r.state.State = "stopped"
			r.state.StopSource = "daemon"
			// Run-scoped report-request state dies with the process anyway;
			// clearing keeps the runner consistent if Shutdown is ever
			// called without exiting (tests do exactly that).
			r.pendingReportSteer = false
			r.reportGraceRounds = 0
			stoppedAtRound = r.state.Round
			name = r.job.Name
		}
		r.mu.Unlock()
		if active {
			r.persistState()
			// Say WHICH job stopped: the loop's silent stop-path return
			// means the journal otherwise records only the daemon's generic
			// shutdown lines (qodo PR#7 finding 1). The audit JSONL and the
			// journal both get the per-job line through the one emit funnel.
			s.emit(name, "stopped", stoppedAtRound, 0, 0, "",
				"daemon shutdown (stop_source=daemon) — re-arm to resume")
		}
		if active && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
	}
	s.wg.Wait()
}
