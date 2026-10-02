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
	mu      sync.Mutex
	buf     strings.Builder
	errMsg  string
	sawEnd  bool
	holdEnd bool // an abort is in flight: the next agent_end is the aborted turn
	drain   bool // hold further control frames until the aborted turn ends
	turnEnd bool // reader flagged the aborted turn's agent_end
	held    []any
	out     *bufio.Writer
	diagf   func(string, ...any)
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
		case "response":
			if e.Success != nil && !*e.Success {
				msg := e.Error
				if msg == "" {
					msg = "command failed"
				}
				s.mu.Lock()
				if s.errMsg == "" {
					s.errMsg = msg
				}
				s.mu.Unlock()
				return
			}
		}
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
	if err := send(frame); err != nil {
		fmt.Fprintf(diagOut, "[control] send failed: %v\n", err)
		c.ack.record(frameID(frame), job.AckSendFail, frameType(frame), err.Error(), 0)
		return
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
