// Package events defines the supervisor's lifecycle events, appends them to a
// per-job JSONL audit trail, and fans them out to live watch subscribers.
// There is no polling consumer (doc/adr/0003-session-notifications.md): the
// only delivery mechanism is a watch client blocked on the control socket,
// which the starting Hermes session arms as a background run.
package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"pi-supervisor/internal/journal"
	"pi-supervisor/internal/taskwatch"
)

// Dir is where the per-job event audit JSONL files live.
func Dir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".pi", "supervisor", "events")
}

// File is the event audit log for one job.
func File(name string) string { return filepath.Join(Dir(), name+".jsonl") }

// Event is one lifecycle record.
type Event struct {
	TS    string `json:"ts"`
	Job   string `json:"job"`
	Event string `json:"event"` // job_started|round_done|instant_exit|backoff|done|fatal|stopped
	Round int    `json:"round,omitempty"`
	RC    int    `json:"rc,omitempty"`
	DurS  int64  `json:"duration_s,omitempty"`
	Text  string `json:"text,omitempty"` // short tail of the round output
	Info  string `json:"info,omitempty"` // human explanation (backoff, fatal)
	// Worktree is the job's cwd, so a waking LLM can run git/verify there.
	Worktree string `json:"worktree,omitempty"`
	// SessionPath is the live pi session transcript (JSONL); the watch client
	// prints its last lines and points at the full file.
	SessionPath string `json:"session_path,omitempty"`
	// PRURL is the pull-request URL scraped from the transcript (ADR-0006).
	// Best-effort / eventually consistent: "" is normal and does not mean no
	// PR was opened, only that the agent did not link one before the round
	// ended.
	PRURL string `json:"pr_url,omitempty"`
	// Task-watch enrichment (ADR-0014): present only on task_completed
	// (Task/TaskFile) and task_lookup_failed (TaskID, ToolCallID, Reason).
	// TaskInfo is the validated, immutable copy from the plugin's JSON —
	// never a title cache or parsed prose.
	TaskID     string              `json:"task_id,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	TaskFile   string              `json:"task_file,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	Task       *taskwatch.TaskInfo `json:"task,omitempty"`
}

// Terminal reports whether an event ends the job's run: `watch` closes the
// stream on it, and `watch -t` exits.
//
// `reviewing` is deliberately NOT terminal (ADR-0012 §4.1): the build job is
// done but the review campaign still owns the round loop, so treating it as
// terminal would stop the watch mid-campaign. `review_done` and
// `review_exhausted` ARE terminal — they are how a campaign ends.
func (e Event) Terminal() bool {
	switch e.Event {
	case "done", "fatal", "stopped", "review_done", "review_exhausted":
		return true
	}
	return false
}

// broker fans events out to watch subscribers. Subscribers get a buffered
// channel and Emit never blocks: a slow or vanished client must not stall a
// round loop. Overflow drops the event for that subscriber — the client's
// next watch picks up state via `status`, and the audit JSONL keeps it for
// humans.
var (
	brokerMu sync.Mutex
	nextSub  int64
	subs     = map[int64]chan Event{}
)

const subBuffer = 16

// Subscribe registers a watcher. It receives every event published from now
// on (all jobs; filtering by job happens at the control layer, which knows
// the requested filter). The returned cancel func must be called on disconnect.
func Subscribe() (id int64, ch <-chan Event, cancel func()) {
	brokerMu.Lock()
	defer brokerMu.Unlock()
	nextSub++
	id = nextSub
	c := make(chan Event, subBuffer)
	subs[id] = c
	journal.Subsys("watch").Info("watch_subscribe", "job", "*")
	return id, c, func() {
		brokerMu.Lock()
		defer brokerMu.Unlock()
		delete(subs, id)
		journal.Subsys("watch").Info("watch_unsubscribe", "job", "*")
	}
}

// releaseKind reports whether event e releases a watch client: the terminal
// set (done|fatal|stopped|review_done|review_exhausted) plus task_completed
// and task_lookup_failed, which release a `watch -t` client per ADR-0014.
func releaseKind(e Event) bool {
	if e.Terminal() {
		return true
	}
	switch e.Event {
	case "task_completed", "task_lookup_failed":
		return true
	}
	return false
}

// publish delivers e to every subscriber without ever blocking.
func publish(e Event) {
	brokerMu.Lock()
	defer brokerMu.Unlock()
	n := len(subs)
	dropped := false
	for _, c := range subs {
		select {
		case c <- e:
		default:
			dropped = true
		}
	}
	if releaseKind(e) {
		journal.Subsys("watch").Info("watch_release", "event_kind", e.Event,
			"job", e.Job, "round", e.Round, "watchers", n)
	}
	if dropped {
		journal.Subsys("watch").Warn("watch_drop", "job", e.Job,
			"event_kind", e.Event)
	}
}

// Emit records one event: audit JSONL append + live fan-out. Errors are
// swallowed by design — an events failure must never break the round loop.
func Emit(e Event) {
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	if err := os.MkdirAll(Dir(), 0o755); err == nil {
		if f, ferr := os.OpenFile(File(e.Job), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); ferr == nil {
			_, _ = f.Write(append(data, '\n'))
			_ = f.Close()
		}
	}
	publish(e)
}
