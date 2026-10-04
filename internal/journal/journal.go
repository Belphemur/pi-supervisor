// Package journal emits one-line structured diagnostics for systemd's
// journal (`journalctl --user -u pi-supervisor -o cat`).
//
// Why: the daemon's own diagnostics used to go only to
// /tmp/pi_<job>_orchestrator.log, which is invisible to systemd and per-job,
// so `systemctl status` showed a single "ready" line and a fatal job was
// silent until someone went looking in /tmp. This package is ADDITIVE: every
// existing /tmp file keeps being written; nothing is moved here.
//
// Format (one record per line, no embedded newlines ever):
//
//	2026-10-04T09:12:33.412Z INFO round event=round_end job=mealime-roomux rc=0 round=3
//	2026-10-04T09:12:34.001Z WARN control event=request_refused cmd=stop job=typo reason=unknown_job err="unknown job \"typo\""
//
// The attribute rendering is zerolog's ConsoleWriter (proper quoting of
// spaces/quotes/empty strings); only the arrangement — timestamp, level,
// subsystem, then event= and key=value — is ours, and the key=value fields
// come out sorted by name so a line is byte-stable for the same record.
//
// Streams (issue #1). journald takes PRIORITY from the STREAM a record
// arrives on, not from anything in the text: with everything on stdout every
// record is PRIORITY 6 (info) and `journalctl -p warning` returns only
// systemd's own unit chatter. So INFO and below go to stdout and WARN and
// above go to stderr, which is what makes the daemon's warnings the visible
// ones. The residual limit is honest and unavoidable here: systemd still
// assigns the whole unit ONE priority, so per-record priorities need a
// /dev/log datagram (issue #1's non-goals). What works today:
//
//	journalctl --user -u pi-supervisor -o cat | grep ' WARN \| ERROR '
//
// SetOutput (tests, or a daemon with stdout on a file) replaces the sink
// wholesale and receives EVERY record, split or not: a redirected sink must
// never silently lose the WARN a test is asserting on.
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
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// SubsystemKey is the attribute that names the emitting subsystem; it is
// hoisted out of the attribute list into the line head.
const SubsystemKey = "subsystem"

// defaultSubsystem labels records emitted without Subsys.
const defaultSubsystem = "daemon"

// tsLayout is ISO-8601 with milliseconds in UTC (2006-01-02T15:04:05.000Z).
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// writeMu serializes whole-line writes across every handler, so concurrent
// job loops and the control server never interleave halves of a line. It is
// per-SINK-independent on purpose: one process-global mutex, so handlers that
// were built against different sinks (a test redirect in flight) still cannot
// splice each other's lines.
var (
	levelVar = new(slog.LevelVar) // INFO until SetLevel
	outMu    sync.Mutex
	writeMu  sync.Mutex
	out      io.Writer = os.Stdout
	sink               = newSink(os.Stdout)
	logger             = slog.New(newLineHandler(sink))
)

// sinkSet is the zerolog writer set. A redirected sink (SetOutput) is a
// single logger that receives EVERY record; only the real daemon split runs
// two loggers so journald can tell INFO from WARN by stream.
type sinkSet struct {
	zl    *zerolog.Logger // unsplit: every level
	out   *zerolog.Logger // stdout: INFO and below
	err   *zerolog.Logger // stderr: WARN and above
	split bool
}

func newSink(w io.Writer) *sinkSet {
	if w != os.Stdout {
		// A test sink, or a daemon whose stdout is a file: one writer for
		// every level, so nothing a test asserts on can be routed away.
		zl := zerolog.New(newConsole(w))
		return &sinkSet{zl: &zl}
	}
	return newSplitSink(os.Stdout, os.Stderr)
}

// newSplitSink is the daemon's real shape: two streams so journald can read a
// priority off them. Split out from newSink so the routing is testable without
// capturing the process's own stdout.
func newSplitSink(out, errw io.Writer) *sinkSet {
	o, e := zerolog.New(newConsole(out)), zerolog.New(newConsole(errw))
	return &sinkSet{out: &o, err: &e, split: true}
}

// newConsole builds the one-line renderer. zerolog's ConsoleWriter supplies
// the quoting (a value with a space, a quote or a control character is
// strconv-quoted, exactly as journalctl's own parser expects).
func newConsole(w io.Writer) *zerolog.ConsoleWriter {
	return &zerolog.ConsoleWriter{
		Out:        w,
		NoColor:    true,
		TimeFormat: tsLayout,
		// No caller: nobody reads the function name, and it would be the
		// third field, before the subsystem.
		PartsOrder: []string{
			zerolog.TimestampFieldName,
			zerolog.LevelFieldName,
			zerolog.MessageFieldName,
		},
		// The timestamp is already formatted in UTC by the caller, so pass it
		// through verbatim instead of re-parsing into the local zone.
		FormatTimestamp: func(i any) string {
			s, _ := i.(string)
			return s
		},
		FormatLevel: func(i any) string {
			s, _ := i.(string)
			if w, ok := levelWords[s]; ok {
				return w
			}
			return strings.ToUpper(s)
		},
		FormatMessage: func(i any) string {
			s, _ := i.(string)
			return s
		},
	}
}

// levelWords is slog's level vocabulary, so a reader who knows `journalctl`
// (or slog) sees what they expect.
var levelWords = map[string]string{
	"trace": "TRACE",
	"debug": "DEBUG",
	"info":  "INFO",
	"warn":  "WARN",
	"error": "ERROR",
	"fatal": "FATAL",
}

// SetOutput redirects the journal (tests, or a daemon started with stdout to
// a file). Safe to call before the first record. It returns the previous sink
// so a caller that redirects — a test, mostly — can put it back instead of
// capturing it before the fact.
func SetOutput(w io.Writer) (prev io.Writer) {
	outMu.Lock()
	defer outMu.Unlock()
	prev, out = out, w
	sink = newSink(w)
	logger = slog.New(newLineHandler(sink))
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

// lineHandler renders one line per record:
//
//	<ts> <LEVEL> <subsystem> event=<slug> key=value ...
//
// slog still owns the record plumbing (levels, With/WithGroup, Enabled) and
// therefore the public API; zerolog owns the encoding, the level-to-stream
// routing and the quoting. only the arrangement of the head is ours, which is
// why the subsystem and the event slug travel as the zerolog message part.
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

func newLineHandler(s *sinkSet) slog.Handler {
	return &writerHandler{level: levelVar, sink: s}
}

// writerHandler owns the destination. There is deliberately NO mutex FIELD
// here: a handler field would be copied by every WithAttrs clone and would
// silently become "one lock per root logger", which is exactly the hole the
// concurrency test could not see. Handle takes the package-global writeMu, so
// every handler in the process — cloned, rebuilt by SetOutput, or built
// directly in a test — serializes on the same lock.
type writerHandler struct {
	lineHandler
	sink *sinkSet
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

	// The head is the message part: subsystem, then event=. Both are quoted
	// with slog's rules so neither a space nor a newline in an event slug can
	// forge a second journal line.
	msg := quote(subsys)
	if r.Message != "" {
		msg += " event=" + quote(r.Message)
	}

	e := h.sink.event(r.Level).Str(zerolog.TimestampFieldName, r.Time.UTC().Format(tsLayout))
	for _, a := range h.attrs {
		if a.Key == SubsystemKey {
			continue // already in the head
		}
		addAttr(e, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == SubsystemKey {
			return true
		}
		addAttr(e, a)
		return true
	})

	writeMu.Lock()
	defer writeMu.Unlock() // one whole-line write at a time
	// Msg() is what actually writes the record, so it belongs inside the lock.
	e.Msg(msg)
	return nil
}

// eventFor picks the event constructor for a level. zerolog filters nothing
// here: the level threshold is the slog LevelVar (Enabled), so there is one
// threshold, not two that can disagree.
//
// The unsplit sink holds ONE logger, so every level must come from it; the
// split sink holds two and routes by level. Getting that backwards is a nil
// dereference in the daemon (zl is nil on the split path), so the shape is
// checked rather than assumed.
func (s *sinkSet) event(l slog.Level) *zerolog.Event {
	if !s.split {
		switch {
		case l < slog.LevelInfo:
			return s.zl.Debug()
		case l < slog.LevelWarn:
			return s.zl.Info()
		case l < slog.LevelError:
			return s.zl.Warn()
		default:
			return s.zl.Error()
		}
	}
	// WARN and ERROR share a stream but NOT a word: collapsing them here
	// would print every refusal as ERROR, and the level token is what an
	// operator greps for.
	switch {
	case l < slog.LevelWarn:
		return s.out.Info()
	case l < slog.LevelError:
		return s.err.Warn()
	default:
		return s.err.Error()
	}
}

// addAttr moves one slog attribute onto the event. Anything zerolog cannot
// render as itself (a duration, a LogValuer, an arbitrary value) becomes a
// string, because a line that drops a field is worse than one that prints it
// in the obvious way.
func addAttr(e *zerolog.Event, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		e.Fields(v.Group())
		return
	}
	if d, ok := v.Any().(time.Duration); ok {
		e.Str(a.Key, d.String())
		return
	}
	e.Any(a.Key, v.Any())
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
