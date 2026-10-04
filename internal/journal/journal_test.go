package journal

import (
	"bytes"
	"log/slog"
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

// The write lock is shared by every handler: two jobs logging at once must
// never produce a line that is a splice of both.
func TestConcurrentLinesDoNotInterleave(t *testing.T) {
	b := capture(t)
	const workers, each = 8, 40
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			log := Subsys("job")
			for i := range each {
				log.Info("round_end", "job", "w", "round", w, "i", i)
			}
		})
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != workers*each {
		t.Fatalf("want %d lines, got %d", workers*each, len(lines))
	}
	for i, line := range lines {
		a := attrs(t, line)
		if a["event"] != "round_end" || a["job"] != "w" || a["round"] == "" || a["i"] == "" {
			t.Fatalf("line %d is spliced: %q (attrs %v)", i, line, a)
		}
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
