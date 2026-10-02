// Package job defines the job model, on-disk layout, and session discovery.
package job

import (
	"encoding/json"
	"errors"
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
}

// Status is the live snapshot served over the socket / written to disk.
type Status struct {
	Name          string  `json:"name"`
	State         string  `json:"state"`
	Round         int     `json:"round"`
	MaxRounds     int     `json:"max_rounds"`
	SessionPath   string  `json:"session_path"`
	SessionBytes  int64   `json:"session_bytes"`
	SessionAgeS   float64 `json:"session_age_s"`
	ClientPID     int     `json:"client_pid"`
	LastRC        int     `json:"last_rc"`
	LastDurS      int64   `json:"last_duration_s"`
	LastRunlogB   int64   `json:"last_runlog_bytes"`
	InstantExits  int     `json:"instant_exits"`
	LastDiag      string  `json:"last_diag"`
	MarkerFound   bool    `json:"marker_found"`
	FinalReportOK bool    `json:"final_report_exists"`
	LastUpdate    string  `json:"last_update"`
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
	defer os.Remove(tmpName) // no-op once the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
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
	if err != nil || len(data) == 0 {
		return ""
	}
	if len(data) > n {
		data = data[len(data)-n:]
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\n", " | "))
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

// FindSession resolves a session JSONL for a job: the newest .jsonl in the
// worktree's munged session dir. Pure Go — no pi_session.py subprocess.
func FindSession(name, worktree string) string {
	dir := MungedSessionsDir(worktree)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if best == "" || fi.ModTime().After(bestMod) {
			best = filepath.Join(dir, e.Name())
			bestMod = fi.ModTime()
		}
	}
	return best
}
