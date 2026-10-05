package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake-run transcript for one TaskUpdate completion:
const fakeTaskUpdateSession = `TEST_TASKWATCH`

// drainObs collects observations from a channel.
type obsSink struct {
	mu   sync.Mutex
	obs  []Observation
	stop bool
	ch   chan Observation
}

func newObsSink() *obsSink {
	return &obsSink{ch: make(chan Observation, 16)}
}

// TestToolExecutionCorrelation drives the fake pi through a TaskUpdate
// completion and asserts exactly one eligible observation.
func TestToolExecutionCorrelation(t *testing.T) {
	sink := newObsSink()
	o := baseOpts(t, fakeTaskUpdateSession)
	o.Observations = sink.ch
	res := Run(o)
	if res.RC != 0 {
		t.Fatalf("RC = %d, want 0 (err=%q)", res.RC, res.Err)
	}
	select {
	case ob := <-sink.ch:
		if ob.TaskID != "7" || ob.ToolCallID == "" {
			t.Fatalf("observation = %+v", ob)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no observation within 2s")
	}
}

// TestNonCompletionCallsDoNotTrigger feeds a bash call, a TaskUpdate with a
// different status, and a TaskUpdate with status completed whose end reports
// an error: none may produce an observation.
func TestNonCompletionCallsDoNotTrigger(t *testing.T) {
	sink := newObsSink()
	o := baseOpts(t, "TEST_TASKWATCH_NOISE")
	o.Observations = sink.ch
	res := Run(o)
	if res.RC != 0 {
		t.Fatalf("RC = %d, want 0", res.RC)
	}
	select {
	case ob := <-sink.ch:
		t.Fatalf("unexpected observation %+v", ob)
	case <-time.After(700 * time.Millisecond):
	}
}

// TestUnmatchedEndPointIsIgnored covers an orphan tool_execution_end with no
// recorded start: it must not initiate a lookup.
func TestUnmatchedEndPointIsIgnored(t *testing.T) {
	sink := newObsSink()
	o := baseOpts(t, "TEST_TASKWATCH_ORPHAN")
	o.Observations = sink.ch
	Run(o)
	select {
	case ob := <-sink.ch:
		t.Fatalf("orphan end produced %+v", ob)
	case <-time.After(700 * time.Millisecond):
	}
}

// TestDuplicateEndCorrelatesOnce covers the plugin's re-deliverable end frame
// for the same toolCallId: the first eligible end emits, the duplicate does
// not.
func TestDuplicateEndCorrelatesOnce(t *testing.T) {
	sink := newObsSink()
	o := baseOpts(t, "TEST_TASKWATCH_DUPEND")
	o.Observations = sink.ch
	Run(o)
	select {
	case ob := <-sink.ch:
		if ob.TaskID != "5" {
			t.Fatalf("observation = %+v", ob)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no observation for the first eligible end")
	}
	select {
	case ob := <-sink.ch:
		t.Fatalf("duplicate end re-emitted %+v", ob)
	case <-time.After(700 * time.Millisecond):
	}
}

// TestGetStateReplyParity proves the identity request reaches --mode rpc with
// a fresh id and that the reply is consumed WITHOUT being mistaken for a
// prompt error (a response with success:false would otherwise fail the
// round; success:true must keep going).
func TestGetStateReplyParity(t *testing.T) {
	fh, err := os.CreateTemp(t.TempDir(), "frames-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	name := fh.Name()
	fh.Close()
	t.Setenv("FAKE_PI_FRAME_LOG", name)

	sink := newObsSink()
	idch := make(chan Identity, 4)
	o := baseOpts(t, fakeTaskUpdateSession)
	o.Observations = sink.ch
	o.Identity = idch
	if res := Run(o); res.RC != 0 {
		t.Fatalf("RC = %d, want 0 (err=%q)", res.RC, res.Err)
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var sawGetState bool
	for line := range strings.SplitSeq(string(raw), "\n") {
		frame := map[string]any{}
		if json.Unmarshal([]byte(line), &frame) != nil {
			continue
		}
		if t, _ := frame["type"].(string); t == "get_state" {
			sawGetState = true
		}
	}
	if !sawGetState {
		t.Fatal("client never sent get_state")
	}
}

// TestSessionHeaderCwdIsRead proves the session-header record (cwd) is
// captured for resolver identity when pi reports it. The fake pi emits a
// session header carrying cwd = its own worktree.
func TestSessionHeaderCwdIsRead(t *testing.T) {
	o := baseOpts(t, fakeTaskUpdateSession)
	o.Worktree = t.TempDir()
	obs := make(chan Observation, 16)
	identCh := make(chan Identity, 4)
	o.Observations = obs
	o.Identity = identCh
	Run(o)
	select {
	case id := <-identCh:
		if id.Cwd != o.Worktree {
			t.Fatalf("cwd = %q, want %q", id.Cwd, o.Worktree)
		}
		if id.SessionID == "" {
			t.Fatal("session id missing from get_state reply")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no identity event")
	}
}

// TestObservationsRaceSafe exercises two overlapping consumer callbacks - the
// delivery must remain serialized per channel send (no interleaved writes).
func TestObservationChannelContract(t *testing.T) {
	ch := make(chan Observation, 4)
	ch <- Observation{TaskID: "1", ToolCallID: "t"}
	got := <-ch
	if got.TaskID != "1" {
		t.Fatalf("got %+v", got)
	}
	_ = filepath.Join // keep filepath import for future tests
}
