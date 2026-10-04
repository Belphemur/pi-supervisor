// Package journal emits one-line structured diagnostics on STDOUT so the
// systemd journal captures them (`journalctl --user -u pi-supervisor -o cat`).
//
// Why: the daemon's own diagnostics used to go only to
// /tmp/pi_<job>_orchestrator.log, which is invisible to systemd and per-job,
// so `systemctl status` showed a single "ready" line and a fatal job was
// silent until someone went looking in /tmp. This package is ADDITIVE: every
// existing /tmp file keeps being written; nothing is moved here.
//
// Format (one record per line, no embedded newlines ever):
//
//	2026-10-04T09:12:33.412Z INFO round event=round_end job=mealime-roomux round=3 rc=0 dur_s=1204
//	2026-10-04T09:12:34.001Z WARN control event=refused cmd=stop job=typo reason=unknown_job error="unknown job \"typo\""
//
// The attribute rendering is the stdlib log/slog TEXT handler (proper quoting
// of spaces/quotes/empty strings); only the head — timestamp, level,
// subsystem — is arranged so `journalctl -o cat` reads left-to-right and the
// event name is the first key=value pair.
//
// Rules this package enforces for its callers (issue #1):
//
//   - UTC timestamps, ISO-8601 with milliseconds.
//   - Transitions and summaries only. Never pi's streamed text, never a
//     control-request payload (a review reply body must never reach the
//     journal), never a credential. Call sites pass symbols, numbers and
//     paths as attributes; the message is a fixed event slug.
//   - The write is a single mutex-guarded syscall on a short line, so it
//     cannot interleave between concurrent jobs and cannot stall a round loop
//     for longer than one line write.
package journal

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

// SubsystemKey is the attribute that names the emitting subsystem; it is
// hoisted out of the attribute list into the line head.
const SubsystemKey = "subsystem"

// defaultSubsystem labels records emitted without Subsys.
const defaultSubsystem = "daemon"

// tsLayout is ISO-8601 with milliseconds in UTC (2006-01-02T15:04:05.000Z).
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// writeMu serializes whole-line writes across every handler, so concurrent
// job loops and the control server never interleave halves of a line.
var (
	levelVar = new(slog.LevelVar) // INFO until SetLevel
	outMu    sync.Mutex
	writeMu  sync.Mutex
	out      io.Writer = os.Stdout
	logger             = slog.New(newLineHandler(&writeMu, os.Stdout, levelVar))
)

// SetOutput redirects the journal (tests, or a daemon started with stdout to
// a file). Safe to call before the first record. It returns the previous sink
// so a caller that redirects — a test, mostly — can put it back instead of
// capturing it before the fact.
func SetOutput(w io.Writer) (prev io.Writer) {
	outMu.Lock()
	defer outMu.Unlock()
	prev, out = out, w
	logger = slog.New(newLineHandler(&writeMu, w, levelVar))
	return prev
}

// SetLevel sets the minimum level emitted.
func SetLevel(l slog.Level) { levelVar.Set(l) }

// Enabled reports whether a level would be emitted. Call sites use it to skip
// building an expensive attribute list (e.g. tailing a diagnostic) when the
// record would be dropped anyway.
func Enabled(l slog.Level) bool { return l >= levelVar.Level() }

// L returns the process-wide logger. Records are prefixed with the default
// subsystem; prefer Subsys.
func L() *slog.Logger { return logger }

// Subsys returns a logger whose records carry `subsystem=name`, rendered as
// the third field of every line.
func Subsys(name string) *slog.Logger { return logger.With(SubsystemKey, name) }

// handler renders one line per record:
//
//	<ts> <LEVEL> <subsystem> event=<msg> key=value ...
//
// slog owns the record plumbing (levels, With/WithGroup, Enabled); only the
// line layout is ours. slog's TEXT handler renders the attributes, so quoting
// and escaping stay stdlib-correct instead of hand-rolled.
type lineHandler struct {
	level slog.Leveler
	// subsys is the resolved subsystem: set by With(SubsystemKey, …) or by the
	// WithAttrs call logger.With(subsys) makes. The attr is stripped from the
	// rendered list so it appears exactly once, in the head.
	subsys string
	// attrs rendered on every line of this logger (rare; Subsys is the only
	// in-tree user).
	attrs  []slog.Attr
	groups []string
}

func newLineHandler(mu *sync.Mutex, w io.Writer, lvl slog.Leveler) slog.Handler {
	return &writerHandler{level: lvl, w: w, mu: mu}
}

// writerHandler owns the destination and the single mutex that keeps
// concurrent emitters (one per job loop plus the control server) from
// interleaving halves of a line.
type writerHandler struct {
	lineHandler
	w  io.Writer
	mu *sync.Mutex
}

func (h *writerHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	n.attrs = make([]slog.Attr, 0, len(h.attrs)+len(as))
	n.attrs = append(n.attrs, h.attrs...)
	n.attrs = append(n.attrs, as...)
	for _, a := range as {
		if a.Key == SubsystemKey {
			if s, ok := a.Value.Any().(string); ok {
				n.subsys = s
			}
		}
	}
	return &n
}

func (h *writerHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.groups = append(append([]string{}, h.groups...), name)
	return &n
}

func (h *writerHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *writerHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level < h.level.Level() {
		return nil
	}
	subsys := h.subsys
	if subsys == "" {
		subsys = defaultSubsystem
	}

	// Rebuild the record without the subsystem attr (it is in the head) and
	// without the message (it becomes event=).
	rec := slog.NewRecord(r.Time, r.Level, "", r.PC)
	for _, a := range h.attrs {
		if a.Key == SubsystemKey {
			continue // already in the head
		}
		rec.AddAttrs(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == SubsystemKey {
			return true
		}
		rec.AddAttrs(a)
		return true
	})

	var attrs bytes.Buffer
	if err := renderAttrs(&attrs, rec); err != nil {
		// A record we cannot render must still be visible; fall back to the
		// event name alone rather than dropping the line silently.
		attrs.Reset()
	}
	line := r.Time.UTC().Format(tsLayout) + " " + levelName(r.Level) + " " + quote(subsys)
	if r.Message != "" {
		line += " event=" + quote(r.Message)
	}
	// slog's text handler terminates with a newline; this handler supplies
	// its own, and two would split every journald record in half.
	if a := strings.TrimRight(attrs.String(), " \n"); a != "" {
		line += " " + a
	}
	line += "\n"

	h.mu.Lock()
	defer h.mu.Unlock() // one whole-line write at a time
	_, err := io.WriteString(h.w, line)
	return err
}

// renderAttrs writes the record's attributes as slog's text handler would,
// minus the time/level/msg keys the head already carries.
func renderAttrs(buf *bytes.Buffer, r slog.Record) error {
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey, slog.LevelKey, slog.MessageKey:
				return slog.Attr{}
			default:
				return a
			}
		},
	})
	return h.Handle(context.Background(), r)
}

// levelName is slog's own short level name, uppercased so `journalctl -o cat`
// lines start with a visually distinct field.
func levelName(l slog.Level) string {
	n := l.String()
	if n == "" {
		return "INFO"
	}
	return strings.ToUpper(n)
}

// quote applies slog text-handler quoting rules to a head field the handler
// renders itself (subsystem / event names).
func quote(s string) string {
	if s == "" {
		return `""`
	}
	needs := false
	for _, r := range s {
		// Control characters count: an unescaped \n in a message would end
		// this line and let the rest forge a second one (journald records are
		// newline-delimited).
		if r == ' ' || r == '"' || r == '=' || r == '\\' || r < 0x20 || r == 0x7f {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	return strconv.Quote(s)
}
