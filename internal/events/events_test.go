package events

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/journal"
)

func testHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// journalCapture points the package sink at a buffer and returns it.
// The journal sink is process-global, so tests that use it cannot run in
// parallel with each other or with journal package tests.
func journalCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	prev := journal.SetOutput(&b)
	t.Cleanup(func() { journal.SetOutput(prev) })
	return &b
}

// field extracts a key=value pair from a journal line, unquoting it.
func field(line, key string) (string, bool) {
	for _, tok := range strings.Fields(line) {
		if !strings.HasPrefix(tok, key+"=") {
			continue
		}
		v := strings.TrimPrefix(tok, key+"=")
		if strings.HasPrefix(v, `"`) {
			u, err := strconv.Unquote(v)
			if err != nil {
				return v, false
			}
			return u, true
		}
		return v, true
	}
	return "", false
}

// hasLine reports whether the captured journal contains a line matching
// every one of the required tokens (space-separated, order-independent).
func hasLine(b *bytes.Buffer, want ...string) bool {
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		ok := true
		for _, w := range want {
			if !strings.Contains(l, w) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Emit must fan out to subscribers and append to the audit JSONL.
func TestEmitFansOutAndAudits(t *testing.T) {
	testHome(t)
	id, ch, cancel := Subscribe()
	defer cancel()
	_ = id

	done := Event{Job: "j1", Event: "done", Round: 3}
	Emit(done)

	select {
	case got := <-ch:
		if got.Event != "done" || got.Job != "j1" {
			t.Fatalf("got %+v, want done/j1", got)
		}
		if got.TS == "" {
			t.Fatal("TS not stamped")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never received the event")
	}

	data, err := os.ReadFile(File("j1"))
	if err != nil {
		t.Fatalf("audit file missing: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("audit line not newline-terminated")
	}
}

// A slow subscriber must never block the publisher: overflow drops, publish
// returns immediately.
func TestPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	testHome(t)
	_, ch, cancel := Subscribe()
	defer cancel()

	start := time.Now()
	for range subBuffer + 10 {
		Emit(Event{Job: "j", Event: "backoff"})
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("publish blocked for %s on a full subscriber", el)
	}
	// The channel holds only subBuffer events.
	if len(ch) > subBuffer {
		t.Fatalf("buffer overflow: %d events queued, cap %d", len(ch), subBuffer)
	}
}

// Cancel stops delivery; no event may arrive after it.
func TestCancelStopsDelivery(t *testing.T) {
	testHome(t)
	_, ch, cancel := Subscribe()
	cancel()
	Emit(Event{Job: "j", Event: "round_done"})
	select {
	case e := <-ch:
		t.Fatalf("received %+v after cancel", e)
	case <-time.After(200 * time.Millisecond):
	}
}

// Multiple subscribers each get their own copy.
func TestFanOutToAllSubscribers(t *testing.T) {
	testHome(t)
	_, ch1, c1 := Subscribe()
	defer c1()
	_, ch2, c2 := Subscribe()
	defer c2()

	Emit(Event{Job: "j", Event: "round_done"})
	for name, ch := range map[string]<-chan Event{"1": ch1, "2": ch2} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %s got nothing", name)
		}
	}
}

// Terminal classification drives the watch's exit logic.
func TestTerminalClassification(t *testing.T) {
	for ev, want := range map[string]bool{
		"done": true, "fatal": true, "stopped": true,
		"round_done": false, "backoff": false, "job_started": false,
		"instant_exit": false,
	} {
		if got := (Event{Event: ev}).Terminal(); got != want {
			t.Fatalf("Terminal(%q) = %v, want %v", ev, got, want)
		}
	}
}

// Audit files land under the events dir keyed by job name.
func TestAuditLayout(t *testing.T) {
	testHome(t)
	if f := File("power-top"); filepath.Base(f) != "power-top.jsonl" {
		t.Fatalf("File = %s", f)
	}
	Emit(Event{Job: "x", Event: "backoff"})
	if _, err := os.Stat(File("x")); err != nil {
		t.Fatalf("audit file not created: %v", err)
	}
}

// 1. Watch client lifecycle logging: subscribe and unsubscribe each produce
// one INFO journal line.
func TestWatchSubscribeUnsubscribeLogging(t *testing.T) {
	testHome(t)
	b := journalCapture(t)

	id, _, cancel := Subscribe()
	defer cancel()

	if !hasLine(b, "event=watch_subscribe", "job=*") {
		t.Fatalf("subscribe did not journal watch_subscribe line; got:\n%s", b.String())
	}

	// Force cancel now (don't wait for defer) to test unsubscribe logging.
	cancel()
	// Create a fresh subscription to then unsubscribe and verify the line.
	_, _, cancel2 := Subscribe()
	cancel2()

	// Count unsubscribe lines: one for the first cancel, one for cancel2.
	unsubCount := 0
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if strings.Contains(l, "event=watch_unsubscribe") {
			unsubCount++
		}
	}
	if unsubCount < 2 {
		t.Fatalf("expected at least 2 watch_unsubscribe lines, got %d; log:\n%s", unsubCount, b.String())
	}

	_ = id
}

// 2. Release-event delivery logging: emitting a release-kind event journals
// watch_release with the current subscriber count, including watchers=0.
func TestWatchReleaseLogsWatchersCount(t *testing.T) {
	testHome(t)
	b := journalCapture(t)

	// With one subscriber: watchers=1.
	_, _, cancel := Subscribe()
	defer cancel()
	Emit(Event{Job: "mealime-search3", Event: "task_completed", Round: 2})

	if !hasLine(b, "event=watch_release", "event_kind=task_completed", "job=mealime-search3", "round=2", "watchers=1") {
		t.Fatalf("release event did not log watch_release with 1 watcher; got:\n%s", b.String())
	}

	// Cancel, then emit a terminal event with zero watchers.
	cancel()
	if !hasLine(b, "event=watch_unsubscribe", "job=*") {
		t.Fatalf("unsubscribe line missing; got:\n%s", b.String())
	}

	// Clear the buffer and emit with no subscribers.
	b.Reset()
	Emit(Event{Job: "mealime-search3", Event: "done", Round: 2})

	if !hasLine(b, "event=watch_release", "event_kind=done", "job=mealime-search3", "round=2", "watchers=0") {
		t.Fatalf("release event with 0 watchers not logged; got:\n%s", b.String())
	}
}

// 3. Drop logging: when a subscriber buffer is full, publish drops and
// journals a WARN watch_drop line.
func TestWatchDropLogs(t *testing.T) {
	testHome(t)
	b := journalCapture(t)

	// Subscribe a client that will never drain its buffer.
	_, ch, cancel := Subscribe()
	defer cancel()

	// Fill the buffer and publish one more: default branch drops it.
	for i := 0; i < subBuffer+1; i++ {
		Emit(Event{Job: "j-drop", Event: "round_done", Round: 1})
	}

	// Verify at least one watch_drop line with correct fields.
	found := false
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if !strings.Contains(l, "event=watch_drop") {
			continue
		}
		found = true
		if !strings.Contains(l, " WARN ") {
			t.Fatalf("watch_drop line is not WARN level: %q", l)
		}
		v, ok := field(l, "event_kind")
		if !ok || v != "round_done" {
			t.Fatalf("watch_drop wrong event_kind: %q", v)
		}
		v, ok = field(l, "job")
		if !ok || v != "j-drop" {
			t.Fatalf("watch_drop wrong job: %q", v)
		}
	}
	if !found {
		t.Fatalf("watch_drop line not found in journal; got:\n%s", b.String())
	}

	// The buffered channel must not exceed its capacity (drop behavior intact).
	if got := len(ch); got > subBuffer {
		t.Fatalf("buffer overflow: %d > %d", got, subBuffer)
	}
}

// 4. watch_release fires per emit for release-kind events regardless of
// subscriber churn (re-arm after a task completion still logs the next one).
func TestWatchReleaseLogsEveryReleaseEvent(t *testing.T) {
	testHome(t)
	b := journalCapture(t)

	_, _, c1 := Subscribe()
	defer c1()
	Emit(Event{Job: "j", Event: "task_lookup_failed", Round: 3})

	_, _, c2 := Subscribe()
	defer c2()
	Emit(Event{Job: "j", Event: "task_completed", Round: 3})

	if !hasLine(b, "event=watch_release", "event_kind=task_lookup_failed", "job=j", "round=3", "watchers=1") {
		t.Fatalf("first release missing; got:\n%s", b.String())
	}
	if !hasLine(b, "event=watch_release", "event_kind=task_completed", "job=j", "round=3", "watchers=2") {
		t.Fatalf("second release missing; got:\n%s", b.String())
	}
}
