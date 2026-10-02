package events

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
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
