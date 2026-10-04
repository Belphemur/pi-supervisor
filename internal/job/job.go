// Package job defines the job model, on-disk layout, and session discovery.
package job

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Home() string { h, _ := os.UserHomeDir(); return h }

func JobsDir() string  { return filepath.Join(Home(), ".pi", "supervisor", "jobs") }
func StateDir() string { return filepath.Join(Home(), ".pi", "supervisor", "state") }
func PiScripts() string {
	return filepath.Join(Home(), ".hermes", "skills", "autonomous-ai-agents", "pi", "scripts")
}

// Job is one supervised delegation. Fields map 1:1 to jobs/<name>.json.
type Job struct {
	Name        string   `json:"name"`
	Brief       string   `json:"brief"`
	Cont        string   `json:"cont"`
	FinalReport string   `json:"final_report"`
	Marker      string   `json:"marker"`
	Worktree    string   `json:"worktree"`
	SessionName string   `json:"session_name"`
	SessionPath string   `json:"session_path"` // pinned/adopted session JSONL
	MaxRounds   int      `json:"max_rounds"`
	TimeoutS    int      `json:"timeout_s"`
	Skills      []string `json:"skills"`
	// PiBin is the pi executable; empty means resolve "pi" on PATH.
	PiBin string `json:"pi_bin,omitempty"`
	// Provider/Model optionally pin pi's backend for this job.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// BackoffScale multiplies every inter-round sleep. 0 means the default
	// (1.0). Tests use a small value to keep round cycles fast; operators
	// can raise it for expensive campaigns.
	BackoffScale float64 `json:"backoff_scale,omitempty"`
	// CI-stall awareness (ADR-0004): the daemon tails the session JSONL for
	// markers that the agent is parked on the CI / code-review loop, and
	// counts one "retry" per quiet park. At CIStallCap stalls (default 3,
	// 0 = default) it interrupts the session with a finish-the-report prompt
	// and closes the run as a failure to finish the review loop.
	// CIStallIdleS is the quiet window that turns an armed marker into a
	// stall (default 300, 0 = default).
	CIStallCap   int `json:"ci_stall_cap,omitempty"`
	CIStallIdleS int `json:"ci_stall_idle_s,omitempty"`
	// EmptyTurnIdleS is the quiet window (seconds) that turns "pi alive but
	// the transcript frozen with zero tool calls" into an empty-turn stall
	// (ADR-0010). 0 = the 60s default. Such a round otherwise burns the whole
	// timeout_s producing nothing, with no error signal anywhere.
	EmptyTurnIdleS int `json:"empty_turn_idle_s,omitempty"`
}

// Paths returns the per-job working files (compat with the bash supervisor's
// names so existing tooling — pi_control.py, greps — keeps working).
func Paths(name string) (ctrl, runlog, orchlog, status string) {
	return "/tmp/pi_" + name + ".ctrl",
		"/tmp/pi_" + name + "_run.log",
		"/tmp/pi_" + name + "_orchestrator.log",
		"/tmp/pi_" + name + "_status.json"
}

func Runlog(name string) string { _, runlog, _, _ := Paths(name); return runlog }
func Ctrl(name string) string   { ctrl, _, _, _ := Paths(name); return ctrl }

// Ack is the per-job steer acknowledgement log (ADR-0005): the client
// appends one AckRecord per control frame it acts on, and `steer` reads its
// own frame's record back so the CLI can report a real delivery outcome.
// The file is truncated together with the ctrl file at every round start.
func Ack(name string) string { return "/tmp/pi_" + name + "_ack.jsonl" }

// Steer ack outcomes. Only AckForwarded/AckSendFail/AckBadFrame are terminal:
// a held frame gets an AckHeld record first and a terminal record later,
// when the abort drain releases it.
const (
	AckForwarded   = "forwarded"   // the frame reached pi's stdin
	AckHeld        = "held"        // queued behind an in-flight abort drain
	AckSendFail    = "send failed" // pi's stdin rejected the frame
	AckBadFrame    = "bad frame"   // the control line was not a JSON object
	AckWritten     = "written"     // --no-wait: on disk, not confirmed
	AckNoRound     = "no live round"
	AckUnconfirmed = "not confirmed"
)

// AckRecord is one line of the ack log (see Ack).
type AckRecord struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Type    string `json:"type,omitempty"`
	Detail  string `json:"detail,omitempty"`
	DelayMS int64  `json:"delay_ms,omitempty"`
	AtMS    int64  `json:"at_ms"`
}

// Terminal reports whether the record ends the steer's wait. Held is not
// terminal: the same frame is acked again once it is actually delivered.
func (a AckRecord) Terminal() bool { return a.Outcome != AckHeld }

// SteerReport is what `pi-supervisor steer` prints: where the frame went and
// what pi did with it.
type SteerReport struct {
	Job         string `json:"job"`
	FrameID     string `json:"frame_id"`
	Round       int    `json:"round"`
	JobState    string `json:"job_state"`
	SessionPath string `json:"session_path"`
	CtrlPath    string `json:"ctrl_path"`
	AckPath     string `json:"ack_path"`
	LiveRound   bool   `json:"live_round"`
	ClientPID   int    `json:"client_pid,omitempty"`
	// Interrupted is true when the steer signaled pi's process group
	// (SIGINT) before writing the frame, so the running turn is asked to
	// stop and pick this message up (ADR-0007). It reports the signal was
	// sent, not that pi obeyed it.
	Interrupted bool   `json:"interrupted"`
	Outcome     string `json:"outcome"`
	Detail      string `json:"detail,omitempty"`
	DelayMS     int64  `json:"delay_ms,omitempty"`
	WaitedMS    int64  `json:"waited_ms"`
	Confirmed   bool   `json:"confirmed"`
}

// MungedSessionsDir is pi's cwd-keyed session directory for a worktree.
func MungedSessionsDir(worktree string) string {
	m := strings.TrimPrefix(worktree, "/")
	m = strings.ReplaceAll(m, "/", "-")
	return filepath.Join(Home(), ".pi", "agent", "sessions", "--"+m+"--")
}

// State is the persisted runtime state (survives daemon restarts).
type State struct {
	Round        int    `json:"round"`
	State        string `json:"state"` // stopped|running|done|fatal
	LastRC       int    `json:"last_rc"`
	LastDurS     int64  `json:"last_duration_s"`
	InstantExits int    `json:"instant_exits"`
	LastDiag     string `json:"last_diag"`
	StartedAt    string `json:"started_at"`
	CIStalls     int    `json:"ci_stalls"` // parks on the CI/review loop, cumulative
	// MarkerSeen latches true once the completion marker has been seen in the
	// session transcript (ADR-0011). Sticky by design: the run log is truncated
	// at the start of every round, so a marker seen in ANY round must survive
	// to the gate — otherwise a finished job livelocks to MaxRounds and ends
	// fatal (exactly what happened to mealime-roomux on PR #43).
	MarkerSeen bool `json:"marker_seen,omitempty"`
	// ReviewBaseline is the post-completion thread baseline (ADR-0012
	// follow-up). It lives on State, NOT on the in-memory reviewCampaign,
	// because its whole purpose is to outlive the campaign: bots re-review the
	// whole diff after every push, so a campaign that closed clean can gain new
	// findings minutes later, with no live round to answer them in. The
	// campaign object is gone by then; this is not.
	ReviewBaseline *ReviewBaseline `json:"review_baseline,omitempty"`
	// PRURL is the first GitHub pull-request URL scraped from this round's
	// transcript (ADR-0006). Best-effort: "" when the agent never linked a
	// PR, even if it opened one.
	PRURL string `json:"pr_url,omitempty"`
}

// ReviewStatus is the review slice of a job's status (ADR-0012). It lives in
// this package because job.Status is the wire shape and the supervisor
// imports job, never the reverse.
type ReviewStatus struct {
	Active   bool   `json:"active"`
	Owner    string `json:"owner,omitempty"`
	Repo     string `json:"repo,omitempty"`
	PR       int    `json:"pr,omitempty"`
	Round    int    `json:"round"`
	MaxRound int    `json:"max_rounds"`
	Type     string `json:"type,omitempty"`
	// PendingAcks is how many bulk_resolve requests await an ack.
	PendingAcks int `json:"pending_acks"`
}

// ReviewBaseline records what the PR looked like when a review campaign ended,
// so a later event-driven check can say "threads appeared after you finished"
// instead of silently losing them. See ADR-0012 follow-up.
type ReviewBaseline struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	PR    int    `json:"pr"`
	// OpenAtClose is the open-thread count observed when the campaign ended.
	OpenAtClose int `json:"open_at_close"`
	// Head is the git SHA that campaign reviewed.
	Head string `json:"head"`
	// ClosedAt is when the baseline was recorded.
	ClosedAt string `json:"closed_at"`
	// OpenNow / NewSinceClose come from the most recent re-check.
	OpenNow       int    `json:"open_now,omitempty"`
	NewSinceClose int    `json:"new_since_close,omitempty"`
	CheckedAt     string `json:"checked_at,omitempty"`
}

// Status is the live snapshot served over the socket / written to disk.
type Status struct {
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Round        int     `json:"round"`
	MaxRounds    int     `json:"max_rounds"`
	SessionPath  string  `json:"session_path"`
	SessionBytes int64   `json:"session_bytes"`
	SessionAgeS  float64 `json:"session_age_s"`
	ClientPID    int     `json:"client_pid"`
	LastRC       int     `json:"last_rc"`
	LastDurS     int64   `json:"last_duration_s"`
	LastRunlogB  int64   `json:"last_runlog_bytes"`
	InstantExits int     `json:"instant_exits"`
	CIStalls     int     `json:"ci_stalls"`
	// Review is the live review campaign's snapshot (ADR-0012), nil when the
	// job is not in a review phase.
	Review *ReviewStatus `json:"review,omitempty"`
	// MarkerSeen latches true once the completion marker has been observed in
	// the session transcript (ADR-0011). It is deliberately sticky: the run
	// log is truncated at the start of every round, so a marker seen in ANY
	// round must survive to the gate — otherwise a finished job livelocks to
	// MaxRounds and ends fatal.
	MarkerSeen bool `json:"marker_seen,omitempty"`
	// PRURL is the pull-request URL found in the last round's transcript
	// (ADR-0006); "" when none was linked.
	PRURL         string `json:"pr_url,omitempty"`
	LastDiag      string `json:"last_diag"`
	MarkerFound   bool   `json:"marker_found"`
	FinalReportOK bool   `json:"final_report_exists"`
	LastUpdate    string `json:"last_update"`
}

// Load parses a job JSON file, applying defaults.
func Load(path string) (Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Job{}, err
	}
	var j Job
	if err := json.Unmarshal(data, &j); err != nil {
		return Job{}, err
	}
	if j.Name == "" {
		j.Name = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	if j.MaxRounds == 0 {
		j.MaxRounds = 200
	}
	if j.TimeoutS == 0 {
		j.TimeoutS = 1800
	}
	if j.SessionName == "" {
		j.SessionName = j.Name
	}
	if j.BackoffScale <= 0 {
		j.BackoffScale = 1.0
	}
	return j, nil
}

// writeAtomic writes data to path via a temp file + fsync + rename, so a crash
// mid-write can never leave a truncated/corrupt state file behind.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// No-op once the rename succeeded; on the error paths below the caller
	// gets the real failure, so the cleanup result is irrelevant.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Save atomically persists a job config (after session-path adoption).
func Save(j Job) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(JobsDir(), j.Name+".json"), data)
}

func SaveState(name string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(StateDir(), name+".json"), data)
}

func LoadState(name string) (State, error) {
	data, err := os.ReadFile(filepath.Join(StateDir(), name+".json"))
	if err != nil {
		return State{}, err
	}
	var st State
	err = json.Unmarshal(data, &st)
	return st, err
}

func Exists(p string) bool { _, err := os.Stat(p); return err == nil }

func Size(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// Tail returns the last n bytes of a file flattened to one line.
func Tail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || n <= 0 {
		// n <= 0 would slice data[len-n:] out of range.
		return ""
	}
	if len(data) > n {
		data = data[len(data)-n:]
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\n", " | "))
}

// TailLines returns the last n complete lines of a JSONL file, each truncated
// to maxChars (with a leading "…" marker when cut). It reads at most the last
// 256KB, so a multi-GB session transcript costs the same as a small one.
// Missing files return nil — callers treat that as "no transcript yet".
//
// A live transcript is being appended to while we read it, so only COMPLETE
// lines are returned: the first line is dropped when the 256KB window starts
// mid-line, and a trailing line without its newline is a torn write and is
// dropped too.
func TailLines(path string, n, maxChars int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }() // read-only handle; close cannot lose data
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return nil
	}
	const window = 256 << 10
	off := max(fi.Size()-int64(window), 0)
	buf := make([]byte, fi.Size()-off)
	got, rerr := f.ReadAt(buf, off)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return nil
	}
	buf = buf[:got]
	complete := off == 0 && (got == 0 || buf[got-1] == '\n')
	if off > 0 {
		// Window starts mid-line: that first line is a fragment, not a line.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			return nil
		}
	}
	if !complete {
		// Torn trailing write (or a truncated window): keep only whole lines.
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			buf = buf[:i+1]
		} else {
			return nil
		}
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if len(ln) > maxChars {
			ln = "…" + ln[len(ln)-maxChars:]
		}
		out = append(out, ln)
	}
	return out
}

// RunlogContains reports whether the marker appears in the run log's last
// 4KB window (same window semantics as the bash supervisor).
func RunlogContains(path, marker string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4096)
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	off := max(fi.Size()-4096, 0)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	return strings.Contains(string(buf), marker)
}

// LastLines returns the final n lines of a file.
func LastLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []string
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	all = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) == 1 && all[0] == "" {
		all = nil
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// FindSession returns the most recently modified session transcript in the
// worktree's session directory, or "" if there is none. Pure Go — no
// pi_session.py subprocess.
//
// notBefore is a LAUNCH-time floor: only files modified at or after it are
// eligible. Pass the zero time to accept any file.
//
// The floor exists because the supervisor discovers a fresh LAUNCH's transcript
// by scanning the directory, and pi creates that file asynchronously — the scan
// routinely runs BEFORE the new file exists. Without a floor the scan returns
// the newest PRE-EXISTING transcript instead, and the caller pins that stale
// path to the job. The job is then welded to a dead transcript: it never grows,
// the completion marker can never appear (ADR-0011), and the empty-turn
// detector aborts a perfectly healthy agent. Observed live on `restart --fresh`,
// which quarantines the old transcript and then re-adopted a different stale
// one from an earlier run, wasting all 5 rounds.
//
// The floor is the structural fix: a file that existed before this round
// launched cannot be this round's session, so it is not a candidate at all.
func FindSession(name, worktree string, notBefore time.Time) string {
	return dirScan(MungedSessionsDir(worktree), notBefore)
}

// sessionScanSlack absorbs filesystem timestamp granularity and clock skew
// between the spawn and the transcript's creation. It is deliberately small:
// large enough to tolerate a same-tick create, far too small to admit a
// transcript from a previous run.
const sessionScanSlack = 2 * time.Second

// Quarantine moves a session JSONL into a _archived-stale/ subdirectory of its
// munged sessions dir, renaming it with a timestamp suffix so it is never
// re-adopted by FindSession (which skips the _archived-stale subdir by
// construction). The original bytes are preserved (os.Rename) and the new
// path is returned. If the source file does not exist, Quarantine is a no-op
// and returns "".
func Quarantine(sessionPath string) (string, error) {
	if sessionPath == "" || !Exists(sessionPath) {
		return "", nil
	}
	dir := filepath.Dir(sessionPath)
	base := filepath.Base(sessionPath)
	qDir := filepath.Join(dir, "_archived-stale")
	if err := os.MkdirAll(qDir, 0o755); err != nil {
		return "", fmt.Errorf("quarantine mkdir %s: %w", qDir, err)
	}
	ts := time.Now().Format("2006-01-02T15-04-05")
	// Guard against same-second collisions.
	root := strings.TrimSuffix(base, filepath.Ext(base))
	ext := filepath.Ext(base)
	name := root + "_" + ts + ext
	dest := filepath.Join(qDir, name)
	for c := 1; Exists(dest); c++ {
		name = fmt.Sprintf("%s_%s_%d%s", root, ts, c, ext)
		dest = filepath.Join(qDir, name)
	}
	if err := os.Rename(sessionPath, dest); err != nil {
		return "", fmt.Errorf("quarantine rename %s -> %s: %w", sessionPath, dest, err)
	}
	return dest, nil
}
