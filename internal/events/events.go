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
}

// Terminal reports whether the event ends the watch (and the job's run).
func (e Event) Terminal() bool {
	return e.Event == "done" || e.Event == "fatal" || e.Event == "stopped"
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
	return id, c, func() {
		brokerMu.Lock()
		defer brokerMu.Unlock()
		delete(subs, id)
	}
}

// publish delivers e to every subscriber without ever blocking.
func publish(e Event) {
	brokerMu.Lock()
	defer brokerMu.Unlock()
	for _, c := range subs {
		select {
		case c <- e:
		default:
		}
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
