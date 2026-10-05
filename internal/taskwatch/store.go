package taskwatch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Scope names the pi-tasks taskScope config values that decide where a store
// lives. Zero value is not used: the resolver defaults to session, exactly
// like pi-tasks' `taskScope ?? "session"`.
type Scope string

const (
	ScopeMemory        Scope = "memory"
	ScopeSession       Scope = "session"
	ScopeSessionGlobal Scope = "session-global"
	ScopeProject       Scope = "project"
)

// Target is the store the plugin would currently persist to. Path == "" and
// Memory == true means memory-only (PI_TASKS=off or taskScope=memory): there
// is NO readable store, and a lookup must fail rather than guess a file.
// Unavailable means "identity is incomplete for this scope right now (e.g. a
// session store before pi created its session file)" — likewise not a
// path to guess with.
type Target struct {
	Path        string
	Memory      bool
	Unavailable bool
	// Why carries the unavailable reason for diagnostics; "" otherwise.
	Why string
	// Scope says which resolution branch answered.
	Scope Scope
}

// IsGlobal reports whether the target came from the session-global scope's
// agent-dir tree (used by tests to assert the scope).
func (t Target) IsGlobal() bool { return t.Scope == ScopeSessionGlobal }

// Resolver mirrors pi-tasks 0.9.0's resolveStoreTarget + tasks-config merge
// for one session identity (ADR-0014 §3). It reads the environment the PI
// CHILD inherited, never the operator shell.
type Resolver struct {
	// Env is the environment map the child actually inherited. Nil means the
	// current process environment.
	Env map[string]string
	// AgentDir overrides <agent-dir> for tests; "" means
	// PI_CODING_AGENT_DIR or ~/.pi/agent.
	AgentDir string
	// TasksDir overrides the named-list home for tests; "" means ~/.pi/tasks.
	TasksDir string
	// SessionID and Cwd come from the running pi session itself (get_state /
	// session header), never from a guess.
	SessionID string
	Cwd       string
}

func (r Resolver) env(key string) string {
	if r.Env != nil {
		return r.Env[key]
	}
	return os.Getenv(key)
}

// TasksDirDefault returns the named-list home: PI_TASKS named lists live at
// ~/.pi/tasks/<name>.json in pi-tasks (task-store.ts pins the dir with
// homedir()), overridable only for tests.
func (r Resolver) tasksDir() string {
	if r.TasksDir != "" {
		return r.TasksDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "tasks")
}

// AgentDir resolves <agent-dir>: constructor value, then
// PI_CODING_AGENT_DIR, then ~/.pi/agent.
func (r Resolver) agentDir() string {
	// PI_CODING_AGENT_DIR wins: the agent dir belongs to the CHILD's
	// environment, and a constructor value is only a test fixture default.
	if v := r.env("PI_CODING_AGENT_DIR"); v != "" {
		return v
	}
	if r.AgentDir != "" {
		return r.AgentDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

// ProjectKey mirrors pi-tasks' projectKey: resolve(cwd), strip the leading
// separator, replace every remaining / \ : with '-', wrap in --…--.
func ProjectKey(cwd string) string {
	abs := cwd
	if abs != "" {
		if a, err := filepath.Abs(abs); err == nil {
			abs = a
		}
	}
	s := strings.TrimLeft(abs, "/\\")
	if strings.HasSuffix(s, "/") || strings.HasSuffix(s, "\\") {
		s = strings.TrimRight(s, "/\\")
	}
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return "--" + replacer.Replace(s) + "--"
}

// workspaceSessionTaskFile mirrors task-paths.ts:
// <cwd>/.pi/tasks/tasks-<sessionId>.json.
func workspaceSessionTaskFile(cwd, sessionID string) string {
	return filepath.Join(cwd, ".pi", "tasks", "tasks-"+sessionID+".json")
}

// globalSessionTasksDir mirrors task-paths.ts:
// <agent-dir>/tasks/sessions/<projectKey(cwd)>.
func (r Resolver) globalSessionTasksDir(cwd string) string {
	return filepath.Join(r.agentDir(), "tasks", "sessions", ProjectKey(cwd))
}

// tasksConfig is the plugin's config file shape; only taskScope matters here
// (the other keys are presentation and never decide the store path).
type tasksConfig struct {
	TaskScope Scope `json:"taskScope"`
}

// readTasksConfig mirrors tasks-config.ts readTasksConfig: unreadable is
// merely empty, and a JSON array (or any non-object) is NOT applied.
func readTasksConfig(path string) tasksConfig {
	data, err := boundedRead(path, maxConfigBytes)
	if err != nil {
		return tasksConfig{}
	}
	var c tasksConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return tasksConfig{}
	}
	return c
}

// maxConfigBytes bounds a tasks-config.json read.
const maxConfigBytes = 1 << 20

// boundedRead reads a regular file up to limit bytes; anything bigger,
// missing, or non-regular is an error. It never opens FIFOs or devices.
func boundedRead(path string, limit int64) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if st.Size() > limit {
		return nil, errors.New("file too large")
	}
	return os.ReadFile(path)
}

// mergeConfig mirrors loadTasksConfig: project overrides global key-by-key;
// here that reduces to taskScope, the only key that decides the path.
func (r Resolver) mergeConfig(cwd string) (Scope, error) {
	global := readTasksConfig(filepath.Join(r.agentDir(), "tasks-config.json"))
	project := readTasksConfig(filepath.Join(cwd, ".pi", "tasks-config.json"))
	scope := global.TaskScope
	if project.TaskScope != "" {
		scope = project.TaskScope
	}
	if scope == "" {
		scope = ScopeSession
	}
	switch scope {
	case ScopeMemory, ScopeSession, ScopeSessionGlobal, ScopeProject:
		return scope, nil
	default:
		return "", errors.New("unknown taskScope " + string(scope))
	}
}

// Resolve returns the store the installed plugin would use right now for
// (Cwd, SessionID). It NEVER creates files and NEVER adopts a fallback for
// an unknown value: ambiguity fails the lookup.
func (r Resolver) Resolve() Target {
	piTasks := r.env("PI_TASKS")
	if piTasks == "off" {
		return Target{Memory: true, Scope: ScopeMemory}
	}
	if piTasks != "" {
		// Absolute or leading-dot: a file path; leading-dot resolves
		// against the ACTIVE session cwd (pi-tasks resolve(cwd, v)).
		if filepath.IsAbs(piTasks) {
			return Target{Path: piTasks, Scope: ScopeProject}
		}
		if strings.HasPrefix(piTasks, ".") {
			if r.Cwd == "" {
				return Target{Unavailable: true, Scope: ScopeProject,
					Why: "PI_TASKS is a relative path but the session cwd is unknown"}
			}
			return Target{Path: filepath.Join(r.Cwd, piTasks), Scope: ScopeProject}
		}
		// Named list: <tasksDir>/<value>.json.
		dir := r.tasksDir()
		if dir == "" {
			return Target{Unavailable: true, Scope: ScopeProject,
				Why: "cannot determine the tasks directory for PI_TASKS named list"}
		}
		return Target{Path: filepath.Join(dir, piTasks+".json"), Scope: ScopeProject}
	}

	// No PI_TASKS: merge configs and follow taskScope. Session scopes
	// additionally require a persisted session, as the plugin does.
	scope, err := r.mergeConfig(r.Cwd)
	if err != nil {
		return Target{Unavailable: true, Scope: ScopeMemory, Why: err.Error()}
	}
	switch scope {
	case ScopeMemory:
		return Target{Memory: true, Scope: scope}
	case ScopeProject:
		if r.Cwd == "" {
			return Target{Unavailable: true, Scope: scope,
				Why: "taskScope project needs the session cwd"}
		}
		return Target{Path: filepath.Join(r.Cwd, ".pi", "tasks", "tasks.json"), Scope: scope}
	case ScopeSession, ScopeSessionGlobal:
		if r.SessionID == "" {
			return Target{Unavailable: true, Scope: scope,
				Why: "taskScope " + string(scope) + " needs the session id"}
		}
		if r.Cwd == "" {
			return Target{Unavailable: true, Scope: scope,
				Why: "taskScope " + string(scope) + " needs the session cwd"}
		}
		ws := workspaceSessionTaskFile(r.Cwd, r.SessionID)
		if scope == ScopeSession {
			return Target{Path: ws, Scope: scope}
		}
		// session-global: an existing workspace file still owns the session
		// (task-paths.ts sessionTaskFile); otherwise the agent-dir tree.
		if _, err := os.Stat(ws); err == nil {
			return Target{Path: ws, Scope: scope}
		}
		return Target{
			Path:  filepath.Join(r.globalSessionTasksDir(r.Cwd), "tasks-"+r.SessionID+".json"),
			Scope: scope,
		}
	}
	// unreachable: mergeConfig validated the scope
	return Target{Unavailable: true, Why: "unreached scope case"}
}

// String renders a Target for diagnostics: identifiers only, never a task
// payload.
func (t Target) String() string {
	switch {
	case t.Memory:
		return "memory"
	case t.Unavailable:
		return "unavailable: " + t.Why
	default:
		return t.Path
	}
}

// TaskInfo is the immutable, validated copy of the plugin's task record that
// travels in events. Field names are preserved (plugin JSON names); arbitrary
// metadata is deliberately excluded (ADR-0014 §4).
type TaskInfo struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	ActiveForm  string   `json:"activeForm,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Blocks      []string `json:"blocks,omitempty"`
	BlockedBy   []string `json:"blockedBy,omitempty"`
	CreatedAtMS int64    `json:"createdAt"`
	UpdatedAtMS int64    `json:"updatedAt"`
}

// Counts reads the CURRENT store and returns (completed, total) plus a
// symbolic failure reason ("" = valid). A valid EMPTY list is 0/0 with NO
// reason; any lookup failure is a fallback 0/0 PLUS the reason (owner
// amendment: never presented as a successful empty list). Same adapter, same
// validation, same bounded reads as Lookup — one parser, two surfaces.
func Counts(path string) (int, int, string) {
	tgt := Target{Path: path}
	snap, reason := readSnapshot(tgt)
	if reason != "" {
		return 0, 0, reason
	}
	completed, total := 0, 0
	for _, t := range snap.Tasks {
		// A task record without a usable ID is plugin-forward data we ignore
		// (same rule as the plugin's own loader).
		if t.ID == "" {
			continue
		}
		total++
		if t.Status == "completed" {
			completed++
		}
	}
	return completed, total, ""
}

// LookupStore is Lookup with the target surfaced, for tests that need to
// assert the store resolution itself (memory never guesses a file).
func LookupStore(path, taskID string) (TaskInfo, bool, string, Target) {
	tgt := Target{Path: path}
	info, found, why := lookup(tgt, taskID)
	return info, found, why, tgt
}

// Lookup reads the CURRENT store on demand (open-by-name, never a held
// handle, so the plugin's atomic rename is honored), validates the
// envelope, and returns (info, ok) plus the symbolic failure reason when not
// ok. Reasons come from internal/fault kinds; the mapping lives in the
// supervisor layer so this package stays free of it.
//
// Bounded reads: oversized/malformed/unsupported files fail safely. No
// locking, repair, or mutation of the plugin's store.
func Lookup(path, taskID string) (TaskInfo, bool, string) {
	tgt := Target{Path: path}
	info, found, why := lookup(tgt, taskID)
	return info, found, why
}

// maxStoreBytes bounds one snapshot read; a legit tasks file is far smaller,
// and 4 MB is generous even for a pathologically large list.
const maxStoreBytes = 4 << 20

// taskJSON is the on-disk record shape we accept (types.ts's Task, minus the
// deliberately-excluded `metadata`). Unknown fields are ignored for forward
// compatibility.
type taskJSON struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	ActiveForm  string   `json:"activeForm"`
	Owner       string   `json:"owner"`
	Blocks      []string `json:"blocks"`
	BlockedBy   []string `json:"blockedBy"`
	CreatedAt   int64    `json:"createdAt"`
	UpdatedAt   int64    `json:"updatedAt"`
}

type snapshotJSON struct {
	NextID int        `json:"nextId"`
	Tasks  []taskJSON `json:"tasks"`
}

// reasons are symbolic; the supervisor maps them onto internal/fault kinds.
const (
	ReasonMissingStore = "missing-store"
	ReasonInvalidData  = "invalid-data"
	ReasonMemoryStore  = "memory-store"
	ReasonMissingTask  = "missing-task"
	ReasonAmbiguousID  = "ambiguous-id"
	ReasonNotCompleted = "not-completed"
	ReasonBadID        = "bad-id"
)

// Counts (resolver form) resolves the CURRENT session's store and counts
// it — the same adapter the enrichment path uses. Unresolved identity or
// memory/off yields the fallback 0/0 plus a reason, never a guessed file.
func (r Resolver) Counts() (int, int, string) {
	tgt := r.Resolve()
	if tgt.Unavailable {
		return 0, 0, ReasonMissingStore
	}
	snap, reason := readSnapshot(tgt)
	if reason != "" {
		return 0, 0, reason
	}
	completed, total := 0, 0
	for _, t := range snap.Tasks {
		if t.ID == "" {
			continue
		}
		total++
		if t.Status == "completed" {
			completed++
		}
	}
	return completed, total, ""
}

// readSnapshot opens the store BY PATH at call time (so the plugin's atomic
// rename is honored), validates the envelope, and returns the snapshot with
// a symbolic reason on failure ("" = ok). Memory/missing targets and unsafe
// file types fail closed; reads are bounded. No locks taken, no writes.
func readSnapshot(tgt Target) (snapshotJSON, string) {
	if tgt.Memory {
		return snapshotJSON{}, ReasonMemoryStore
	}
	if tgt.Path == "" {
		return snapshotJSON{}, ReasonMissingStore
	}
	st, err := os.Stat(tgt.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return snapshotJSON{}, ReasonMissingStore
		}
		return snapshotJSON{}, ReasonInvalidData
	}
	if !st.Mode().IsRegular() {
		// FIFO/device/dir: reading could hang or mislead. Fail closed.
		return snapshotJSON{}, ReasonInvalidData
	}
	if st.Size() > maxStoreBytes {
		return snapshotJSON{}, ReasonInvalidData
	}
	data, err := os.ReadFile(tgt.Path)
	if err != nil {
		return snapshotJSON{}, ReasonInvalidData
	}
	var snap snapshotJSON
	if err := json.Unmarshal(data, &snap); err != nil || snap.Tasks == nil {
		return snapshotJSON{}, ReasonInvalidData
	}
	return snap, ""
}

// lookup is Lookup's body, testable against explicit targets.
func lookup(tgt Target, taskID string) (TaskInfo, bool, string) {
	if taskID == "" {
		return TaskInfo{}, false, ReasonBadID
	}
	snap, reason := readSnapshot(tgt)
	if reason != "" {
		return TaskInfo{}, false, reason
	}
	var matches []TaskInfo
	for _, t := range snap.Tasks {
		if t.ID != taskID {
			continue
		}
		matches = append(matches, TaskInfo{
			ID:          t.ID,
			Subject:     t.Subject,
			Description: t.Description,
			Status:      t.Status,
			ActiveForm:  t.ActiveForm,
			Owner:       t.Owner,
			Blocks:      t.Blocks,
			BlockedBy:   t.BlockedBy,
			CreatedAtMS: t.CreatedAt,
			UpdatedAtMS: t.UpdatedAt,
		})
	}
	switch len(matches) {
	case 0:
		return TaskInfo{}, false, ReasonMissingTask
	case 1:
		if matches[0].Status != "completed" {
			return TaskInfo{}, false, ReasonNotCompleted
		}
		return matches[0], true, ""
	default:
		return TaskInfo{}, false, ReasonAmbiguousID
	}
}
