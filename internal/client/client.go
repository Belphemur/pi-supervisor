// Package client is the daemon-native pi RPC client. It owns the
// `pi --mode rpc` subprocess: LF-delimited JSON framing on stdin/stdout,
// streamed text capture, control-file steering, and the abort-drain protocol.
//
// It replaces pi_rpc_client.py (see doc/adr/0002-daemon-native-client-and-status.md)
// with identical semantics so the supervisor's exit classification and
// backoff logic see the same signals: rc 0 on agent_end, rc 1 on error or
// timeout.
package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"pi-supervisor/internal/job"
)

// Options configures one RPC round.
type Options struct {
	// PiBin is the pi executable (default: "pi", resolved via PATH).
	PiBin string
	// Session resumes an existing session JSONL; when empty a new session is
	// launched with Name in Worktree.
	Session  string
	Name     string
	Worktree string
	// Skills are passed as repeated --skill flags.
	Skills []string
	// Provider/Model override pi's defaults when set.
	Provider string
	Model    string
	// Prompt is the full message body for this round.
	Prompt string
	// ControlPath, when set, is polled for new JSONL frames to forward to
	// pi's stdin (steering). The whole file is read from the start: the
	// supervisor truncates it at every round start, so its entire contents
	// belong to this round (a steer written between the truncate and the
	// spawn must not be lost).
	ControlPath string
	// AckPath, when set, receives one job.AckRecord per control frame this
	// client acts on (ADR-0005) so `steer` can report a real outcome.
	AckPath string
	// PollInterval overrides the control-file poll period; 0 = PollInterval.
	PollInterval time.Duration
	// Timeout is the round deadline; on expiry an abort frame is sent and
	// the client gives the turn GracePeriod to end before reporting rc 1.
	Timeout time.Duration
	// GracePeriod is how long to wait for the aborted turn's agent_end.
	GracePeriod time.Duration
	// Out receives streamed assistant text (the run log).
	Out io.Writer
	// Diag receives control-protocol diagnostics (also the run log).
	Diag io.Writer
	// OnPID, when set, is called with pi's pid as soon as it starts, so the
	// supervisor can expose/kill the process group mid-round.
	OnPID func(int)
	// Observations, when set, receives one completion observation per eligible
	// TaskUpdate execution (ADR-0014): toolName == "TaskUpdate",
	// args.status == "completed", a matching non-error tool_execution_end, and
	// deduped per execution identity. Round-scoped state; the channel is the
	// only handoff so the reader goroutine never blocks the supervisor.
	Observations chan<- Observation
	// Identity, when set, receives the session identity (get_state reply plus
	// session-header cwd) once known. At most one send per round; an unsend
	// keeps the round unaffected (identity is best-effort).
	Identity chan<- Identity
}

// Result reports how the round ended.
type Result struct {
	// RC: 0 = agent_end, 1 = error/timeout/spawn failure, 2 = stdout closed
	// without agent_end.
	RC int
	// Duration is wall time of the round.
	Duration time.Duration
	// Text is the full streamed assistant output.
	Text string
	// Err is the failure reason when RC != 0.
	Err string
	// PID is the pi subprocess pid (process-group leader).
	PID int
}

// Observation is one eligible TaskUpdate execution that ended without error
// (ADR-0014 §2): the task JSON still has to confirm the completion before
// anything is published. Execution identity is the pi toolCallId.
type Observation struct {
	TaskID     string `json:"task_id"`
	ToolCallID string `json:"tool_call_id"`
}

// Identity is the running session's identity, from pi itself (never guessed):
// the get_state reply carries sessionId/SessionFile, the session header the
// cwd. Either half may be absent when pi had not produced it yet.
type Identity struct {
	SessionID   string `json:"session_id"`
	SessionFile string `json:"session_file"`
	Cwd         string `json:"cwd"`
}

// PollInterval is how often the control file is checked for new frames.
// `steer`'s ack wait is bounded by it (plus a margin), so it is exported.
const PollInterval = 5 * time.Second

// Run executes one RPC round to completion.
//
// The caller is responsible for killing the process group on hard timeout;
// Run handles its own soft deadline (abort frame + grace) and always reaps pi.
func Run(o Options) Result {
	start := time.Now()
	res := Result{Duration: 0}

	if o.PiBin == "" {
		o.PiBin = "pi"
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.GracePeriod <= 0 {
		o.GracePeriod = 30 * time.Second
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Diag == nil {
		o.Diag = io.Discard
	}

	cmd := exec.Command(o.PiBin, rpcArgs(o)...)
	cmd.Dir = o.Worktree
	if cmd.Dir == "" {
		cmd.Dir = "."
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(start, res, fmt.Sprintf("stdin pipe: %v", err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(start, res, fmt.Sprintf("stdout pipe: %v", err))
	}
	// pi's own stderr is noise for our purposes; the client is the only
	// thing that writes to the run log.
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fail(start, res, fmt.Sprintf("spawn %s: %v", o.PiBin, err))
	}
	res.PID = cmd.Process.Pid
	if o.OnPID != nil {
		o.OnPID(res.PID)
	}

	// Buffered writer so per-token writes don't become a syscall storm; a
	// flusher keeps the run log live for the marker gate and for humans.
	// The flusher shares the stream's mutex: bufio.Writer is not safe for
	// concurrent Flush + WriteString.
	out := bufio.NewWriterSize(o.Out, 32*1024)
	st := &stream{
		out: out,
		// Diagnostics are best-effort by construction: the diag sink IS the
		// error channel, so there is nowhere to report a failure to write to
		// it. The round's real result travels in res (RC/Text/Duration).
		diagf: func(f string, a ...any) { _, _ = fmt.Fprintf(o.Diag, f+"\n", a...) },
	}
	if o.Observations != nil {
		st.observations = o.Observations
		st.pending = map[string]Observation{}
	}
	if o.Identity != nil {
		st.identity = o.Identity
		st.cwd = o.Worktree
	}
	flusherDone := make(chan struct{})
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-flusherDone:
				return
			case <-t.C:
				st.flush()
			}
		}
	}()

	sendMu := sync.Mutex{}
	send := func(frame any) error {
		b, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		sendMu.Lock()
		defer sendMu.Unlock()
		if _, err := stdin.Write(append(b, '\n')); err != nil {
			return err
		}
		return nil
	}

	// Prompt first, exactly like the Python client.
	if err := send(map[string]any{"id": "r1", "type": "prompt", "message": o.Prompt}); err != nil {
		close(flusherDone)
		st.flush()
		_ = reap(cmd, 5*time.Second)
		return fail(start, res, fmt.Sprintf("send prompt: %v", err))
	}
	st.setBusy(true)
	// ADR-0014: ask for session identity once per round. The reply is a
	// `response` frame correlated by command; the reader consumes it without
	// touching the error path (only success:false fails a round).
	if o.Identity != nil {
		if err := send(map[string]any{"id": "tw-state", "type": "get_state"}); err != nil {
			// Identity is best-effort: a failed send is diagnosed and the
			// round continues, so a taskwatch regression can never fail a
			// pi round.
			st.diagf("[taskwatch] get_state send failed: %v", err)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		st.read(stdout)
	}()

	ctl := newControlReader(o.ControlPath, o.AckPath, st.diagf, o.PollInterval)
	deadline := time.Now().Add(o.Timeout)

	for {
		select {
		case <-done:
			// Round finished on its own terms.
			st.flush()
			close(flusherDone)
			_ = stdin.Close()
			_ = reap(cmd, 10*time.Second)
			res.Duration = time.Since(start)
			res.Text = st.text()
			switch {
			case st.errMsg != "":
				return fail(start, res, st.errMsg)
			case st.sawEnd:
				res.RC = 0
				return res
			default:
				// stdout closed without agent_end: pi died mid-turn.
				res.RC = 2
				return res
			}

		case <-ctl.tick():
			ctl.pump(send, st, o.Diag)

		case <-time.After(time.Until(deadline)):
			// One-shot escalation (ADR-0002): abort, then hold the drain
			// window for the aborted turn's agent_end, then reap and fail.
			// There is no second attempt — the deadline has been reached, so
			// re-arming it would only extend an already-expired round.
			st.diagf("timeout at %ds — sending abort", int(time.Since(start).Seconds()))
			_ = send(map[string]any{"type": "abort"})
			// The aborted turn may still emit agent_end; wait for the drain
			// window rather than killing mid-write.
			select {
			case <-done:
			case <-time.After(o.GracePeriod):
			}
			st.flush()
			close(flusherDone)
			_ = stdin.Close()
			_ = reap(cmd, 5*time.Second)
			res.Duration = time.Since(start)
			res.Text = st.text()
			return fail(start, res, "TIMEOUT")
		}
	}
}

func rpcArgs(o Options) []string {
	args := []string{"--mode", "rpc"}
	if o.Session != "" {
		args = append(args, "--session", o.Session)
	} else if o.Name != "" {
		args = append(args, "-n", o.Name)
	}
	for _, s := range o.Skills {
		args = append(args, "--skill", s)
	}
	if o.Provider != "" {
		args = append(args, "--provider", o.Provider)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	return args
}

func fail(start time.Time, res Result, msg string) Result {
	res.RC = 1
	res.Err = msg
	res.Duration = time.Since(start)
	return res
}

// reap waits up to grace for pi to exit on its own, then SIGTERM/SIGKILLs the
// process group so no orphan survives the round. A short grace is correct on
// the timeout path: pi already received its abort frame and drained.
func reap(cmd *exec.Cmd, grace time.Duration) error {
	if grace <= 0 {
		grace = 15 * time.Second
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return <-done
		}
	}
}

// stream correlates pi's JSONL event stream.
type stream struct {
	mu     sync.Mutex
	buf    strings.Builder
	errMsg string
	// steerErr holds a refused STEER's error: diagnostic only, never the
	// round's verdict (see the success:false handler below).
	steerErr string
	sawEnd   bool
	holdEnd  bool // an abort is in flight: the next agent_end is the aborted turn
	drain    bool // hold further control frames until the aborted turn ends
	turnEnd  bool // reader flagged the aborted turn's agent_end
	busy     bool // a turn is in flight: prompt forwarded, agent_end not yet seen
	held     []any
	out      *bufio.Writer
	diagf    func(string, ...any)
	// taskwatch state (ADR-0014): pending completions keyed by toolCallId,
	// plus the identity/observation handoffs. All guarded by mu; sends are
	// non-blocking so the reader never stalls on a slow consumer.
	pending      map[string]Observation
	observations chan<- Observation
	identity     chan<- Identity
	identSent    bool
	cwd          string // the child's working directory (identity, ADR-0014)
}

func (s *stream) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var e struct {
			Type                  string `json:"type"`
			AssistantMessageEvent struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			} `json:"assistantMessageEvent"`
			Success *bool  `json:"success"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			continue // non-JSON noise: ignore, matching the Python reader
		}
		switch e.Type {
		case "message_update":
			if e.AssistantMessageEvent.Type == "text_delta" {
				s.mu.Lock()
				s.buf.WriteString(e.AssistantMessageEvent.Delta)
				if s.out != nil {
					// Run-log mirroring is best-effort: the authoritative
					// text is s.buf (returned as Result.Text), so a failed
					// mirror cannot change the round's classification.
					_, _ = s.out.WriteString(e.AssistantMessageEvent.Delta)
				}
				s.mu.Unlock()
			}
		case "agent_end":
			s.mu.Lock()
			s.busy = false // the turn ended; a plain prompt is deliverable again
			if s.holdEnd {
				// This agent_end closes the ABORTED turn, not our job.
				s.turnEnd = true
				s.mu.Unlock()
				s.diagf("[control] aborted turn ended — delivering interrupt message")
				continue
			}
			s.sawEnd = true
			s.mu.Unlock()
			return
		case "tool_execution_start", "tool_execution_end", "response":
			// Decoded once for the correlation (and get_state); the same line
			// feeds the round's error path below.
			tf, _ := decodeToolExec(line)
			switch e.Type {
			case "tool_execution_start":
				s.recordStart(tf)
			case "tool_execution_end":
				s.recordEnd(tf)
			case "response":
				s.recordResponse(tf)
			}
			if e.Success != nil && !*e.Success {
				msg := e.Error
				if msg == "" {
					msg = "command failed"
				}
				s.mu.Lock()
				// Correlate the refusal to the frame that caused it (kody
				// PR#9 round 6): the round's own prompt is id "r1"; every
				// control-file steer carries its own id. A refusal of a
				// steer — "Agent is already processing", a stale id, a
				// transient provider hiccup — is not the round's verdict:
				// the round's prompt ran and the turn may be healthy.
				// Failing the round on a steer refusal is what let one bad
				// reminder kill a whole campaign round (flambette#65
				// rounds 7-8). The id — not a turn-state flag — is the
				// discriminator, because busy is momentarily false in the
				// abort-drain window between the aborted turn's agent_end
				// and the held prompt's re-send.
				if tf.ID != "" && tf.ID != "r1" && s.steerErr == "" && strings.Contains(msg, "already processing") {
					s.steerErr = msg
					s.mu.Unlock()
					s.diagf("[control] steer %s refused by pi (%s); the steer is dropped, the round continues", tf.ID, msg)
					continue
				}
				if s.errMsg == "" {
					s.errMsg = msg
				}
				s.mu.Unlock()
				return
			}
		}
	}
}

// toolExecFrame is the subset of tool_execution_start/end (and the get_state
// response) the correlation needs, decoded once at the boundary.
type toolExecFrame struct {
	Type       string
	ToolCallID string
	ToolName   string
	Args       json.RawMessage
	IsError    bool
	// get_state response fields
	Command string
	Success bool
	Data    json.RawMessage
	// response correlation: the RPC frame's id, so a refusal can be tied
	// to the frame that caused it (r1 = the round's own prompt; steer ids
	// carry the steer- prefix from the supervisor).
	ID string
}

// decodeToolExec parses one raw tool-execution (or response) frame.
func decodeToolExec(line []byte) (toolExecFrame, bool) {
	var raw struct {
		Type       string          `json:"type"`
		ID         string          `json:"id"`
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		Args       json.RawMessage `json:"args"`
		IsError    bool            `json:"isError"`
		Command    string          `json:"command"`
		Success    *bool           `json:"success"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return toolExecFrame{}, false
	}
	f := toolExecFrame{
		Type:       raw.Type,
		ID:         raw.ID,
		ToolCallID: raw.ToolCallID,
		ToolName:   raw.ToolName,
		Args:       raw.Args,
		IsError:    raw.IsError,
		Command:    raw.Command,
		Data:       raw.Data,
	}
	if raw.Success != nil {
		f.Success = *raw.Success
	}
	return f, true
}

// recordStart tracks an eligible TaskUpdate completion request keyed by its
// execution id (ADR-0014 §2.1). Anything else is not a trigger.
func (s *stream) recordStart(e toolExecFrame) {
	var args struct {
		TaskID string `json:"taskId"`
		Status string `json:"status"`
	}
	var eligible bool
	if e.ToolName == "TaskUpdate" {
		if err := json.Unmarshal(e.Args, &args); err == nil &&
			args.TaskID != "" && args.Status == "completed" {
			eligible = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return
	}
	if !eligible {
		// A non-eligible call re-using the same id (retry, pi-side) must not
		// leave the earlier trigger armed.
		delete(s.pending, e.ToolCallID)
		return
	}
	s.pending[e.ToolCallID] = Observation{TaskID: args.TaskID, ToolCallID: e.ToolCallID}
}

// recordEnd fires the observation for a matching, non-error end exactly once.
// Orphan ends (no recorded start), error ends and unrelated tools are dropped:
// none of them may initiate a task-store lookup (ADR-0014 §2.3).
func (s *stream) recordEnd(e toolExecFrame) {
	s.mu.Lock()
	if s.pending == nil {
		s.mu.Unlock()
		return
	}
	ob, ok := s.pending[e.ToolCallID]
	if ok {
		// Dedupe by EXECUTION identity: a duplicate end frame for the same
		// call is not a new observation.
		delete(s.pending, e.ToolCallID)
	}
	s.mu.Unlock()
	if !ok || e.IsError {
		return
	}
	s.deliverObservation(ob)
}

// deliverObservation hands one observation off without ever blocking the
// reader: a supervisor that is not draining must not stall the goroutine that
// has to keep receiving pi's frames (ADR-0014 §5).
func (s *stream) deliverObservation(ob Observation) {
	if s.observations == nil {
		return
	}
	select {
	case s.observations <- ob:
		s.diagf("[taskwatch] eligible completion: task %s call %s", ob.TaskID, ob.ToolCallID)
	default:
		s.diagf("[taskwatch] observation dropped (consumer not draining): task %s", ob.TaskID)
	}
}

// recordResponse consumes a correlated get_state reply for session identity
// (ADR-0014 §2). Identity is one-shot per round; a get_state FAILURE still
// reaches the round's error path through the caller's success check, so only
// success=true replies with data are consumed here. Auxiliary state replies
// are never prompt errors nor steer acks: this method reads and leaves.
func (s *stream) recordResponse(e toolExecFrame) {
	if e.Command != "get_state" || !e.Success {
		return
	}
	var data struct {
		SessionID   string `json:"sessionId"`
		SessionFile string `json:"sessionFile"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		s.diagf("[taskwatch] get_state data unreadable: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.identSent || s.identity == nil {
		return
	}
	s.identSent = true
	// Cwd: pi's RPC stream has NO session header (docs/json.md), so the
	// session's cwd is pi's own working directory — Options.Worktree, which
	// Run set as cmd.Dir. That IS the environment the child inherited.
	id := Identity{SessionID: data.SessionID, SessionFile: data.SessionFile, Cwd: s.cwd}
	select {
	case s.identity <- id:
	default:
		s.diagf("[taskwatch] identity event dropped (consumer not draining)")
	}
}

// flush drains the run-log buffer under the stream lock.
func (s *stream) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out != nil {
		_ = s.out.Flush()
	}
}

func (s *stream) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.buf.String())
}

// setBusy records whether a turn is in flight (prompt forwarded, agent_end
// not yet seen). Mid-turn prompt frames need pi's streamingBehavior field —
// a plain prompt while processing is rejected ("Agent is already processing")
// and that error was killing whole rounds (flambette#65 campaign, 2026-10-08).
// Every prompt SEND goes through markPromptSent and every turn END (agent_end,
// either kind) through markTurnEnded, so busy tracks the actual wire state —
// including the replacement turn an interrupt-steer starts, which a reader-
// only lifecycle missed (the replacement turn's prompt is sent by pump, not
// by the r1 path, so its agent_end would otherwise leave busy stuck false).
func (s *stream) setBusy(b bool) {
	s.mu.Lock()
	s.busy = b
	s.mu.Unlock()
}

// markPromptSent records that a prompt frame just went out on stdin: from this
// instant a turn is (or is again) in flight until its agent_end arrives.
func (s *stream) markPromptSent() {
	s.mu.Lock()
	s.busy = true
	s.mu.Unlock()
}

// processing reports whether a turn is believed in flight.
func (s *stream) processing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// forwardable reports whether a control frame may be sent to pi now, and
// releases frames held during an abort drain once the turn has ended.
func (s *stream) forwardable(frameType string) (send bool, hold bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drain {
		if frameType != "abort" {
			return false, true
		}
		// An abort during a drain: let it through and keep waiting.
		s.holdEnd = true
		return true, false
	}
	if frameType == "abort" {
		s.holdEnd = true
		s.drain = true
	}
	return true, false
}

// releaseDrain flushes frames held during an abort once the aborted turn ended.
func (s *stream) releaseDrain() (ready bool, held []any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.drain || !s.turnEnd {
		return false, nil
	}
	s.drain, s.turnEnd, s.holdEnd = false, false, false
	return true, s.held
}

// hold queues a frame for delivery after the abort drain.
func (s *stream) hold(frame any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = append(s.held, frame)
}

// controlReader tails the steer file and turns new lines into RPC frames.
type controlReader struct {
	path    string
	ack     *acker
	fh      *os.File
	pos     int64
	partial string // trailing bytes of a line that was not fully written yet
	ticker  *time.Ticker
	diagf   func(string, ...any)
	started bool
	// heldAt records when each frame was queued behind an abort drain, in
	// the same order the frames were held, so the release loop can report
	// how long a steer waited (ADR-0005).
	heldAt []time.Time
}

// tick returns a channel that fires every pollInterval. A nil channel (no
// control path) blocks forever, which is exactly what select wants.
func (c *controlReader) tick() <-chan time.Time {
	if c.ticker == nil {
		return nil
	}
	return c.ticker.C
}

func newControlReader(path, ackPath string, diagf func(string, ...any), interval time.Duration) *controlReader {
	c := &controlReader{path: path, diagf: diagf, ack: &acker{path: ackPath}}
	if interval <= 0 {
		interval = PollInterval
	}
	if path == "" {
		return c
	}
	// Open read-only and start at the beginning: the supervisor truncates
	// the ctrl file at every round start, so everything in it is a steer
	// meant for THIS round. Starting at the end (as an earlier version did)
	// silently dropped a frame written between the truncate and the spawn.
	fh, err := os.Open(path)
	if err != nil {
		// No file yet is fine — steer() creates it on demand.
		c.ticker = time.NewTicker(interval)
		return c
	}
	c.fh = fh
	c.started = true
	c.ticker = time.NewTicker(interval)
	return c
}

// pump forwards any new control-file lines into pi's stdin.
func (c *controlReader) pump(send func(any) error, st *stream, diagOut io.Writer) {
	if c.fh == nil {
		return
	}
	buf := make([]byte, 64*1024)
	n, err := c.fh.ReadAt(buf, c.pos)
	if n > 0 {
		c.pos += int64(n)
		// A frame is only valid once its newline has landed: a steer that
		// straddles a 64KB read boundary would otherwise be reported as a
		// bad frame AND lose the rest. Keep the tail until the rest arrives.
		data := c.partial + string(buf[:n])
		whole := ""
		if i := strings.LastIndexByte(data, '\n'); i >= 0 {
			whole, c.partial = data[:i], data[i+1:]
		} else {
			// No newline yet: a frame larger than the read buffer can never
			// complete, so drop the runaway instead of growing forever.
			if len(data) > 1<<20 {
				c.partial = ""
			} else {
				c.partial = data
			}
		}
		for ln := range strings.SplitSeq(whole, "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			c.deliver(ln, send, st, diagOut)
		}
	}
	if err != nil && !os.IsNotExist(err) && c.started {
		// File rotated/truncated underneath us: restart from the beginning.
		if fi, serr := os.Stat(c.path); serr == nil && fi.Size() < c.pos {
			c.pos, c.partial = 0, ""
		}
	}
	if ready, frames := st.releaseDrain(); ready {
		for _, f := range frames {
			at := c.popHeldAt()
			if err := send(f); err != nil {
				fmt.Fprintf(diagOut, "[control] send failed: %v\n", err)
				c.ack.record(frameID(f), job.AckSendFail, frameType(f), err.Error(), 0)
				continue
			}
			if frameType(f) == "prompt" {
				// qodo PR#9 finding 1: this prompt STARTS the replacement
				// turn after the interrupt — busy must rise here or a
				// further mid-turn prompt would go out unmarked and be
				// rejected.
				st.markPromptSent()
			}
			fmt.Fprintf(diagOut, "[control] delivered after abort: %s\n", frameType(f))
			delay := time.Duration(0)
			if !at.IsZero() {
				delay = time.Since(at)
			}
			c.ack.record(frameID(f), job.AckForwarded, frameType(f),
				"queued behind an abort; delivered once the drain settled", delay)
		}
	}
}

// deliver forwards one control-file line to pi and records what happened
// (ADR-0005) so `steer` can report the real outcome instead of assuming.
func (c *controlReader) deliver(ln string, send func(any) error, st *stream, diagOut io.Writer) {
	var frame map[string]any
	if jerr := json.Unmarshal([]byte(ln), &frame); jerr != nil {
		_, _ = fmt.Fprintf(diagOut, "[control] bad frame skipped: %v\n", jerr)
		// No id to match on: the line never parsed. Record the raw prefix so
		// the operator can tell which steer was dropped.
		c.ack.record("", job.AckBadFrame, "?", truncForAck(ln), 0)
		return
	}
	if _, held := st.forwardable(frameType(frame)); held {
		st.hold(frame)
		_, _ = fmt.Fprintf(diagOut, "[control] held until abort settles: %s\n", frameType(frame))
		c.heldAt = append(c.heldAt, time.Now())
		c.ack.record(frameID(frame), job.AckHeld, frameType(frame), "queued behind an in-flight abort", 0)
		return
	}
	// A prompt reaching pi MID-TURN must carry streamingBehavior: a plain
	// prompt while processing is rejected outright ("Agent is already
	// processing") and that rejection surfaced as a client error, killing
	// the whole round (flambette#65 campaign rounds 7-8). "steer" interjects
	// the message into the live turn without aborting it — the ADR-0019
	// reminder's exact purpose — and an idle agent gets the frame as-is,
	// matching the original r1 prompt shape.
	if frameType(frame) == "prompt" && st.processing() {
		if _, ok := frame["streamingBehavior"]; !ok {
			frame["streamingBehavior"] = "steer"
		}
	}
	if err := send(frame); err != nil {
		fmt.Fprintf(diagOut, "[control] send failed: %v\n", err)
		c.ack.record(frameID(frame), job.AckSendFail, frameType(frame), err.Error(), 0)
		return
	}
	if frameType(frame) == "prompt" {
		// The send succeeded: a turn is now in flight (started or
		// restarted), whatever the reader goroutine has consumed so far.
		// Tracking the WIRE state, not the read state, is what keeps the
		// enrichment honest across interrupt boundaries.
		st.markPromptSent()
	}
	fmt.Fprintf(diagOut, "[control] forwarded: %s\n", frameType(frame))
	c.ack.record(frameID(frame), job.AckForwarded, frameType(frame), "", 0)
}

// popHeldAt returns (and forgets) the enqueue time of the oldest held frame.
func (c *controlReader) popHeldAt() time.Time {
	if len(c.heldAt) == 0 {
		return time.Time{}
	}
	at := c.heldAt[0]
	c.heldAt = c.heldAt[1:]
	return at
}

func truncForAck(s string) string {
	const maxLen = 120
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// acker appends AckRecords to the per-job ack log. Opened lazily so a round
// that is never steered leaves no file behind, and written with one syscall
// per record so a record is durable the moment `steer` reads it.
type acker struct {
	path string
	f    *os.File
}

func (a *acker) record(id, outcome, typ, detail string, delay time.Duration) {
	if a == nil || a.path == "" {
		return
	}
	if a.f == nil {
		f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			a.path = "" // unopenable: stop retrying on every frame
			return
		}
		a.f = f
	}
	line, err := json.Marshal(job.AckRecord{
		ID:      id,
		Outcome: outcome,
		Type:    typ,
		Detail:  detail,
		DelayMS: delay.Milliseconds(),
		AtMS:    time.Now().UnixMilli(),
	})
	if err != nil {
		return
	}
	_, _ = a.f.Write(append(line, '\n'))
}

// frameID returns a frame's id, or "" when it has none. Steer always injects
// one so its ack record can be matched back to it.
func frameID(f any) string {
	if m, ok := f.(map[string]any); ok {
		if id, ok := m["id"].(string); ok {
			return id
		}
	}
	return ""
}

func frameType(f any) string {
	if m, ok := f.(map[string]any); ok {
		if t, ok := m["type"].(string); ok {
			return t
		}
	}
	return "?"
}

// DefaultPromptFile resolves a prompt that may be either inline text or a
// path to a file containing the message.
func DefaultPromptFile(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty prompt")
	}
	if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
		// A short single-line value is treated as inline text, matching how
		// the Python client distinguished -f FILE from PROMPT.
		if st.Size() < 4096 && !strings.ContainsAny(p, "\n") && filepath.Ext(p) == "" {
			return p, nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return p, nil
}
