package journal

import (
	"bytes"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// capture points the package sink at a buffer and restores it afterwards.
// The package logger is process-global, so tests that touch it cannot run in
// parallel with each other.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	prev := SetOutput(&b)
	t.Cleanup(func() { SetOutput(prev) })
	return &b
}

// fields splits a log line into its head (everything before the first
// space-separated token that is not the event= pair) and the tail.
func attrs(t *testing.T, line string) map[string]string {
	t.Helper()
	_, rest, ok := strings.Cut(line, " ")
	if !ok {
		t.Fatalf("line has no head: %q", line)
	}
	m := map[string]string{}
	for _, f := range tokenize(rest) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(v, `"`) {
			m[k] = v
			continue
		}
		unq, err := strconv.Unquote(v)
		if err != nil {
			t.Fatalf("field %q is not a valid quoted value: %v", f, err)
		}
		m[k] = unq
	}
	return m
}

// tokenize splits on spaces but keeps a quoted run in one piece — the same
// rule journalctl's own parser applies.
func tokenize(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		for i < len(s) && s[i] != ' ' {
			if s[i] == '"' {
				i++ // opening quote
				for i < len(s) && s[i] != '"' {
					if s[i] == '\\' {
						i++
					}
					i++
				}
				i++ // closing quote
				continue
			}
			i++
		}
		out = append(out, s[start:i])
	}
	return out
}

// The contract the whole daemon log is read against: one line, UTC
// millisecond timestamp, LEVEL, subsystem, then event= and key=value pairs.
// If this format changes, every journalctl query in the runbook changes too.
func TestLineFormat(t *testing.T) {
	b := capture(t)
	Subsys("round").Info("round_end", "job", "mealime", "round", 3, "rc", 0)

	got := b.String()
	if n := strings.Count(got, "\n"); n != 1 {
		t.Fatalf("want exactly one newline, got %d in %q", n, got)
	}
	line := strings.TrimSuffix(got, "\n")
	head := line
	for _, tok := range []string{" INFO ", " round ", " event=round_end "} {
		if !strings.Contains(line, tok) {
			t.Fatalf("line %q lacks %q", line, tok)
		}
		head = strings.Replace(head, tok, " ", 1)
	}
	if len(head) < 24 || head[4] != '-' || head[10] != 'T' || head[23] != 'Z' {
		t.Fatalf("timestamp is not ISO-8601 UTC with ms: %q", head)
	}
	a := attrs(t, line)
	if a["job"] != "mealime" || a["round"] != "3" || a["rc"] != "0" {
		t.Fatalf("attrs = %v", a)
	}
	if a["event"] != "round_end" {
		t.Fatalf("event attr = %q", a["event"])
	}
}

// journalctl parses whitespace-separated tokens, so a value containing a
// space must be quoted or it becomes two bogus fields.
func TestValueWithSpaceIsQuoted(t *testing.T) {
	b := capture(t)
	Subsys("control").Warn("refused", "cmd", "stop", "job", "ty po", "reason", "unknown_job")

	line := strings.TrimSuffix(b.String(), "\n")
	if !strings.Contains(line, `job="ty po"`) {
		t.Fatalf("value with a space was not quoted: %q", line)
	}
	a := attrs(t, line)
	if a["job"] != "ty po" || a["reason"] != "unknown_job" {
		t.Fatalf("quoted value did not round-trip: %v", a)
	}
	if a["event"] != "refused" {
		t.Fatalf("event = %q", a["event"])
	}
}

func TestLevelAndLevelName(t *testing.T) {
	b := capture(t)
	l := Subsys("daemon")
	l.Debug("hidden")
	l.Info("shown")
	l.Warn("warned")
	l.Error("failed")

	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (INFO default drops debug), got %d:\n%s", len(lines), b.String())
	}
	for i, want := range []string{" INFO ", " WARN ", " ERROR "} {
		if !strings.Contains(lines[i], want) {
			t.Fatalf("line %d = %q, want level %q", i, lines[i], want)
		}
	}
}

func TestSetLevelFilters(t *testing.T) {
	b := capture(t)
	SetLevel(slog.LevelWarn)
	t.Cleanup(func() { SetLevel(slog.LevelInfo) })
	Subsys("daemon").Info("quiet")
	Subsys("daemon").Warn("loud")

	if n := strings.Count(b.String(), "\n"); n != 1 {
		t.Fatalf("want 1 line, got %d:\n%s", n, b.String())
	}
	if !strings.Contains(b.String(), "event=loud") {
		t.Fatalf("WARN was dropped: %q", b.String())
	}
	if !Enabled(slog.LevelWarn) || Enabled(slog.LevelInfo) {
		t.Fatal("Enabled disagrees with the configured level")
	}
}

// With on the logger (the Subsys path) must produce the subsystem in the
// head exactly once — never doubled, never leaking into the attr list.
func TestSubsystemAppearsOnce(t *testing.T) {
	b := capture(t)
	Subsys("review").Error("bulk_resolve_requested", "job", "xpoint", "threads", 12)

	line := strings.TrimSuffix(b.String(), "\n")
	if n := strings.Count(line, "review"); n != 1 {
		t.Fatalf("subsystem %q appears %d times in %q", "review", n, line)
	}
	a := attrs(t, line)
	if _, dup := a[SubsystemKey]; dup {
		t.Fatalf("subsystem leaked into the attr list: %v", a)
	}
}

// yieldSink is a destination whose Write is NOT atomic: it copies half the
// bytes, yields the processor, then copies the rest — what a pipe, a tty or a
// journald fd can do to a large write, and what two concurrent writers do to
// each other in any case. It is the honest adversary for "one whole-line
// write at a time": with the shared lock the two halves can never be split by
// another handler; without it, they are.
type yieldSink struct{ b bytes.Buffer }

func (y *yieldSink) Write(p []byte) (int, error) {
	const minLen = 256
	if len(p) < minLen {
		return y.b.Write(p)
	}
	half := len(p) / 2
	if _, err := y.b.Write(p[:half]); err != nil {
		return 0, err
	}
	runtime.Gosched() // the tear window: a scheduling point, never a sleep
	if _, err := y.b.Write(p[half:]); err != nil {
		return half, err
	}
	return len(p), nil
}

// The write lock is shared by every handler, and that is the invariant: in
// production three distinct subsystems (daemon, job, control) hold three
// different handlers writing into ONE sink, so a per-handler mutex would let
// them splice each other's lines. The test therefore logs from all three with
// payloads far larger than one write, through a sink that can tear — deleting
// the shared lock makes records merge/truncate and this fails. A test that used
// one subsystem (one handler, which would still hold one lock even without the
// shared one) passes with the lock deleted, which is exactly the hole this
// replaces.
//
// No sleeps: a start barrier lines the goroutines up and the adversarial sink
// supplies the tear window, so -race and -count=N are stable.
func TestConcurrentLinesDoNotInterleave(t *testing.T) {
	sink := &yieldSink{}
	prev := SetOutput(sink)
	t.Cleanup(func() { SetOutput(prev) })
	const (
		subsystems         = 3
		workersPerSubsys   = 4
		each               = 20
		payloadRepeatBytes = 4096
	)
	seen := map[string]bool{}
	subs := []string{"daemon", "job", "control"}
	total := subsystems * workersPerSubsys * each

	var start sync.WaitGroup // barrier: all goroutines leave together
	var done sync.WaitGroup
	start.Add(1)
	for s := range subsystems {
		for w := range workersPerSubsys {
			done.Go(func() {
				log := Subsys(subs[s])
				tag := strconv.Itoa(s*100 + w)
				// Every record carries its own marker at both ends plus a
				// payload big enough that a torn write cannot resynchronize
				// on a line boundary by luck.
				blob := strings.Repeat("x", payloadRepeatBytes)
				start.Wait()
				for i := range each {
					log.Info("round_end",
						"subsys", subs[s], "tag", tag, "i", strconv.Itoa(i),
						"head", tag+"-"+strconv.Itoa(i),
						"blob", blob,
						"tail", tag+"-"+strconv.Itoa(i)+"-end")
				}
			})
		}
	}
	start.Done()
	done.Wait()

	lines := strings.Split(strings.TrimSuffix(sink.b.String(), "\n"), "\n")
	if len(lines) != total {
		t.Fatalf("want %d intact lines, got %d: records were merged or lost "+
			"(the shared write lock is gone?)", total, len(lines))
	}
	for i, line := range lines {
		a := attrs(t, line)
		if a["event"] != "round_end" {
			t.Fatalf("line %d lost its event: %q (attrs %v)", i, line, a)
		}
		head, tail := a["head"], a["tail"]
		if head == "" || head+"-end" != tail {
			t.Fatalf("line %d is spliced: head %q tail %q", i, head, tail)
		}
		if got := strings.Count(a["blob"], "x"); got != payloadRepeatBytes {
			t.Fatalf("line %d payload is %d bytes, want %d: torn write",
				i, got, payloadRepeatBytes)
		}
		// The tag must be one this run issued, and each (tag,i) exactly once.
		key := a["tag"] + "/" + a["i"]
		if a["subsys"] == "" || seen[key] {
			t.Fatalf("line %d has duplicate/unknown tag: %q (attrs %v)", i, key, a)
		}
		seen[key] = true
	}
	if len(seen) != total {
		t.Fatalf("saw %d distinct records, want %d", len(seen), total)
	}
}

// A message that looks like an injected log line must not be able to forge
// one: only the level word and the subsystem are unquoted single tokens, so
// a newline in the message would otherwise start a second line.
func TestMessageCannotForgeALine(t *testing.T) {
	b := capture(t)
	Subsys("daemon").Info("ok\n2026-01-01T00:00:00.000Z ERROR fake forged",
		"job", "x")

	got := b.String()
	if n := strings.Count(got, "\n"); n != 1 {
		t.Fatalf("message injected a line break:\n%q", got)
	}
	if !strings.Contains(got, `event="ok\n2026-01-01T00:00:00.000Z ERROR fake forged"`) {
		t.Fatalf("message was not quoted/escaped: %q", got)
	}
}

// The split sink is the daemon's real shape and the ONLY place the level
// word and the stream are decided, so it gets its own test: INFO must land on
// stdout, WARN and ERROR on stderr, and they must keep their own words.
// Regression guard for a collapse that turned every refusal into ERROR.
func TestSplitSinkRoutesByLevel(t *testing.T) {
	var out, errb bytes.Buffer
	h := &writerHandler{level: levelVar, sink: newSplitSink(&out, &errb)}
	lg := slog.New(h)

	lg.With(SubsystemKey, "job").Info("probe_info")
	lg.With(SubsystemKey, "control").Warn("probe_warn")
	lg.With(SubsystemKey, "job").Error("probe_error")

	oLines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	eLines := strings.Split(strings.TrimSuffix(errb.String(), "\n"), "\n")
	if len(oLines) != 1 || !strings.Contains(oLines[0], " INFO ") ||
		!strings.Contains(oLines[0], "job event=probe_info") {
		t.Fatalf("stdout stream = %q", out.String())
	}
	if len(eLines) != 2 {
		t.Fatalf("stderr stream = %q", errb.String())
	}
	if !strings.Contains(eLines[0], " WARN ") || !strings.Contains(eLines[0], "control event=probe_warn") {
		t.Fatalf("WARN line = %q", eLines[0])
	}
	if !strings.Contains(eLines[1], " ERROR ") || !strings.Contains(eLines[1], "event=probe_error") {
		t.Fatalf("ERROR line = %q", eLines[1])
	}
}
