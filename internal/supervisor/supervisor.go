// Package supervisor runs the per-job round loops: drives the daemon-native
// pi RPC client (internal/client), classifies exits, adaptive backoff, marker
// gate, instant-exit strikes.
package supervisor

import (
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
	"pi-supervisor/internal/job"
	"pi-supervisor/internal/stall"
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
	// reviewRounds is the default campaign budget (ADR-0012 §5); 0 = 5.
	reviewRounds int
	// reviewAckTimeoutDur bounds a pending bulk_resolve; 0 = 30m.
	reviewAckTimeoutDur time.Duration
	// reviewWarmupDur is how long the auto-trigger waits for CodeRabbit to
	// produce threads before counting; 0 = 5m.
	reviewWarmupDur time.Duration
}

type runner struct {
	mu      sync.Mutex
	job     job.Job
	state   job.State
	pid     int
	started time.Time
	runlogB int64
	stopCh  chan struct{}
	active  bool
	// review is the live review campaign, if any (ADR-0012 §4). Separate from
	// state so a review round never resets the build job's accounting.
	review *reviewCampaign
	// autoReview is the one-shot intent to arm a review when the completion
	// gate closes on an open PR.
	autoReview *autoReviewSpec
}

func New() *Supervisor {
	return &Supervisor{
		jobs:      map[string]*runner{},
		stop:      make(chan struct{}),
		ghClients: newReviewClients(),
	}
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
	defer r.mu.Unlock()
	st := job.Status{
		Name: r.job.Name, State: r.state.State, Round: r.state.Round,
		MaxRounds: r.job.MaxRounds, SessionPath: r.job.SessionPath,
		ClientPID: r.pid, LastRC: r.state.LastRC, LastDurS: r.state.LastDurS,
		LastRunlogB: r.runlogB, InstantExits: r.state.InstantExits,
		CIStalls: r.state.CIStalls,
		LastDiag: r.state.LastDiag, PRURL: r.state.PRURL,
		LastUpdate: time.Now().Format(time.RFC3339),
	}
	if r.job.FinalReport != "" {
		st.FinalReportOK = job.Exists(r.job.FinalReport)
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
	// Truthful mid-round, not just after classification (ADR-0011).
	st.MarkerFound = r.state.State == "done" || r.state.MarkerSeen
	return st
}

func (s *Supervisor) Status(name string) (any, error) {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown job %q", name)
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
		return fmt.Errorf("unknown job %q", name)
	}
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return fmt.Errorf("job %q already running", name)
	}
	if r.state.State == "done" {
		r.mu.Unlock()
		return fmt.Errorf("job %q already done; clear state to rerun", name)
	}
	r.active = true
	r.state.State = "running"
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
		return fmt.Errorf("unknown job %q", name)
	}
	r.mu.Lock()
	active := r.active
	if active {
		// Mark inactive synchronously under the lock so a second Stop() is a
		// no-op rather than a double-close of the stop channel.
		r.active = false
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
	}
	pid := r.pid
	// Round the stop landed on, for the event payload. Read under the same lock
	// as the mutation above so it cannot disagree with what was persisted.
	stoppedAtRound := r.state.Round
	r.mu.Unlock()
	if !active {
		return fmt.Errorf("job %q not running", name)
	}
	r.persistState()
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
		return fmt.Errorf("unknown job %q", name)
	}
	r.mu.Lock()
	r.state.Round = 0
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
		return job.SteerReport{Job: name}, fmt.Errorf("unknown job %q", name)
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
		return rep, fmt.Errorf("%s", rep.Detail)
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
		r.mu.Unlock()
		if round > maxRounds {
			r.mu.Lock()
			name := r.job.Name
			r.mu.Unlock()
			// A campaign that exhausts its budget is a distinct failure from a
			// build job that never reached its marker, and the message has to
			// say which: "round cap reached without marker" would be a lie for
			// a review round (there is no marker in a review round).
			if reviewing {
				r.finish("fatal", "review round cap reached with threads still open")
				s.logf(name, "FATAL: review round cap %d reached without a clean review", maxRounds)
				s.emit(name, "review_exhausted", round, 0, 0, "",
					"review round cap %d reached with threads still open — re-arm with `pi-supervisor review %s --pr N`",
					maxRounds, name)
				return
			}
			r.finish("fatal", "round cap reached without marker")
			s.logf(name, "FATAL: round cap %d reached without marker", maxRounds)
			s.emit(name, "fatal", round, 0, 0, "", "round cap %d reached without marker", maxRounds)
			return
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
		close(markerStop)
		<-markerExited // completion watcher likewise
		// Adopt the transcript a fresh LAUNCH just created, so the gate below
		// has a surface to read on the very round that started the session.
		if sess == "" {
			r.mu.Lock()
			sess = r.job.SessionPath
			r.mu.Unlock()
			if sess == "" {
				if found := job.FindSession(j.SessionName, j.Worktree); found != "" {
					sess = found
					r.mu.Lock()
					r.job.SessionPath = found
					r.mu.Unlock()
					_ = job.Save(r.job)
				}
			}
		}
		select {
		case <-stopCh:
			r.mu.Lock()
			r.state.State, r.state.LastRC, r.active = "stopped", rc, false
			name := r.job.Name
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "loop stopped at round %d (client rc=%d)", round, rc)
			s.emit(name, "stopped", round, rc, dur, "", "operator stop")
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
		s.logf(name, "round %d: client exit=%d duration=%ds runlog=%dB", round, rc, dur, runlogB)
		if r.stateSnapshot().LastDiag != "" {
			s.logf(name, "round %d diagnostic: %s", round, r.stateSnapshot().LastDiag)
		}

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
		markerSeen := r.stateSnapshot().MarkerSeen
		if !markerSeen && j.Marker != "" && sess != "" {
			markerSeen = job.TranscriptContains(sess, j.Marker)
		}
		if !markerSeen && j.Marker != "" {
			markerSeen = job.RunlogContains(job.Runlog(name), j.Marker)
		}
		if j.Marker != "" && markerSeen && job.Exists(j.FinalReport) {
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
			}
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "round %d: marker %q detected in session transcript — done", round, j.Marker)
			if autoArmed {
				// Emit `reviewing`, NOT `done`. `done` is a TERMINAL event
				// (events.Event.Terminal), so a `watch` client would print
				// "THE RUN IS OVER", exit 0, and stop listening while the
				// campaign is still running. The build verdict is preserved
				// on state for the final report; the LIVE verdict is the
				// campaign's.
				s.emit(name, "reviewing", round, 0, dur, "",
					"marker %q reached — build job done, entering the review campaign (ADR-0012 §4.1)",
					j.Marker)
				return
			}
			s.emit(name, "done", round, 0, dur, "",
				"marker %q detected in session transcript — run is over", j.Marker)
			return
		}

		// A CI-stall cap intervention may have closed the run mid-round
		// (ADR-0004). The done-check above already had its chance to upgrade
		// a finished report to done; otherwise the fatal stands.
		if r.stateSnapshot().State == "fatal" {
			s.logf(name, "round %d: run closed by CI-stall intervention", round)
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
			p := job.FindSession(j.SessionName, j.Worktree)
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
			r.state.State, r.active = "stopped", false
			r.mu.Unlock()
			r.persistState()
			s.emit(name, "stopped", round, rc, dur, "", "operator stop during backoff")
			return
		case <-time.After(sleep):
		}
	}
}

func (r *runner) stateSnapshot() job.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// live reports whether a round is currently polling the control file.
func (r *runner) live() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active && r.pid > 0
}

func (r *runner) finish(state, diag string) {
	r.mu.Lock()
	r.state.State, r.state.LastDiag, r.active = state, diag, false
	name := r.job.Name
	r.mu.Unlock()
	r.persistState()
	_ = name
}

// round runs one pi RPC round via the daemon-native client (internal/client);
// hard-kills the pi process group at timeout+120s as a backstop.
func (s *Supervisor) round(r *runner, round int, stopCh chan struct{}) (rc int, dur int64, runlogB int64) {
	r.mu.Lock()
	j := r.job
	resume := j.SessionPath != ""
	r.mu.Unlock()

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

	if resume {
		s.logf(j.Name, "round %d: RESUME %s", round, j.SessionPath)
	} else {
		s.logf(j.Name, "round %d: LAUNCH brief=%s", round, j.Brief)
	}

	out, err := os.OpenFile(runlog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 1, 0, 0
	}
	// The run log is a diagnostic mirror; the round's verdict comes from
	// the client Result, so a failed close (append-only handle) is not fatal.
	defer func() { _ = out.Close() }()

	promptPath := j.Brief
	if resume {
		promptPath = j.Cont
	}
	prompt, err := client.DefaultPromptFile(promptPath)
	if err != nil {
		s.logf(j.Name, "round %d: prompt unreadable: %v", round, err)
		return 1, 0, 0
	}

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
			path = job.FindSession(r.job.SessionName, r.job.Worktree)
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
			// not lost to the tick boundary.
			if mw := ensure(); mw != nil {
				mw.Poll()
				s.setMarkerSeen(r, mw.Seen(), round)
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
			interruptErr := s.interruptWith(name, interrupt)
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
			err := s.interruptWith(name, msg)
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
// the client's abort-drain handshake consumes.
func (s *Supervisor) interruptWith(name, text string) error {
	f, err := os.OpenFile(job.Ctrl(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close() // control file: the frames are already written below
	rid := fmt.Sprintf("%d", time.Now().UnixNano())
	for _, frame := range []any{
		map[string]any{"id": "abort-" + rid, "type": "abort"},
		map[string]any{"id": "int-" + rid, "type": "prompt", "message": text},
	} {
		line, _ := json.Marshal(frame)
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// emit records one lifecycle event (audit JSONL + live fan-out to watches).
// info is printf-formatted for readable diagnostics. The job's worktree is
// attached so a waking LLM can target verification at the right path.
func (s *Supervisor) emit(jobName, event string, round, rc int, durS int64, text, format string, args ...any) {
	if len(text) > 200 {
		text = text[len(text)-200:]
	}
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
	events.Emit(events.Event{
		Job: jobName, Event: event, Round: round, RC: rc, DurS: durS,
		Text: text, Info: info, Worktree: wt, SessionPath: sess, PRURL: pr,
	})
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
			case "done", "fatal", "stopped":
				cancel()
				ev := events.Event{
					TS: time.Now().UTC().Format(time.RFC3339), Job: jobName,
					Event: snap.State, Round: snap.Round, RC: snap.LastRC,
					DurS: snap.LastDurS,
					Info: "run already " + snap.State + " — nothing to wait for",
				}
				r.mu.Lock()
				ev.Worktree = r.job.Worktree
				ev.SessionPath = r.job.SessionPath
				ev.PRURL = r.state.PRURL
				r.mu.Unlock()
				return nil, func() {}, &ev
			}
		}
	}
	_ = id
	return ch, cancel, nil
}

// RunningCount reports how many jobs are currently in a running round. Fed
// to systemd as STATUS= on every watchdog beat.
func (s *Supervisor) RunningCount() int {
	s.mu.Lock()
	runners := make([]*runner, 0, len(s.jobs))
	for _, r := range s.jobs {
		runners = append(runners, r)
	}
	s.mu.Unlock()
	n := 0
	for _, r := range runners {
		r.mu.Lock()
		if r.active && r.state.State == "running" {
			n++
		}
		r.mu.Unlock()
	}
	return n
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
		if active {
			// Same guarded close as Stop(): a concurrent operator Stop() must
			// not turn this into a double-close of the channel.
			r.active = false
			close(r.stopCh)
		}
		r.mu.Unlock()
		if active && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
	}
	s.wg.Wait()
}
