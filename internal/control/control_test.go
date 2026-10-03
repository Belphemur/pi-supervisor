package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pi-supervisor/internal/job"

	"pi-supervisor/internal/events"
)

// fakeHandler is a scriptable Handler: each call can be made to fail, and
// every call is recorded so dispatch routing can be asserted.
type fakeHandler struct {
	mu     sync.Mutex
	calls  []string
	failOn map[string]error
	logs   []string
	events chan events.Event
	// pre is returned by Watch as the "already terminal" precheck event.
	pre *events.Event
}

func newFakeHandler() *fakeHandler {
	return &fakeHandler{
		failOn: map[string]error{},
		events: make(chan events.Event, 8),
	}
}

func (f *fakeHandler) record(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return f.failOn[name]
}

func (f *fakeHandler) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeHandler) Status(name string) (any, error) {
	if err := f.record("status:" + name); err != nil {
		return nil, err
	}
	return map[string]any{"job": name}, nil
}

func (f *fakeHandler) StatusAll() any {
	_ = f.record("statusall")
	return []any{map[string]any{"job": "a"}}
}

func (f *fakeHandler) Start(name string) error { return f.record("start:" + name) }
func (f *fakeHandler) Stop(name string) error  { return f.record("stop:" + name) }
func (f *fakeHandler) Restart(name string) error {
	return f.record("restart:" + name)
}
func (f *fakeHandler) Steer(name, text string, noWait, interrupt bool) (job.SteerReport, error) {
	if err := f.record(fmt.Sprintf("steer:%s:%s:%t:%t", name, text, noWait, interrupt)); err != nil {
		return job.SteerReport{Job: name}, err
	}
	return job.SteerReport{Job: name, FrameID: "steer-1", Outcome: job.AckForwarded, Confirmed: true, Interrupted: interrupt}, nil
}

func (f *fakeHandler) Logs(name string, n int) ([]string, error) {
	if err := f.record(fmt.Sprintf("logs:%s:%d", name, n)); err != nil {
		return nil, err
	}
	return f.logs, nil
}

func (f *fakeHandler) Reload() error { return f.record("reload") }

// watchHandler adds the Watcher push extension on top of fakeHandler. It is a
// separate type so the "watch unsupported" test can serve a plain Handler.
type watchHandler struct{ *fakeHandler }

// Watch implements Watcher.
func (f *watchHandler) Watch(job string) (<-chan events.Event, func(), *events.Event) {
	_ = f.record("watch:" + job)
	if f.pre != nil {
		return nil, func() {}, f.pre
	}
	// The cancel closure runs on the watcher's own goroutine, possibly after
	// the test returned, so its bookkeeping error cannot fail the test.
	return f.events, func() { _ = f.record("cancel:" + job) }, nil
}

func startServer(t *testing.T, h Handler) (sock string, stop chan struct{}) {
	t.Helper()
	sock = filepath.Join(t.TempDir(), "c.sock")
	stop = make(chan struct{})
	go Serve(sock, h, stop)
	// Wait for the listener to appear rather than sleeping a fixed amount.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			return sock, stop
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("control server never came up")
	return "", nil
}

// ask sends one request line and reads the single response line.
func ask(t *testing.T, sock, raw string) Response {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Test client socket: a close error after the assertions cannot
	// fail the test, so it is deliberately ignored.
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte(raw + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		t.Fatalf("no response to %q: %v", raw, err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("bad response %q: %v", string(line), err)
	}
	return resp
}

// A steer always answers with its report, success or failure: "no live
// round" and "cannot write" are exactly the facts the operator needs.
func TestSteerAlwaysReturnsItsReport(t *testing.T) {
	h := newFakeHandler()
	sock, stop := startServer(t, h)
	defer close(stop)

	resp := ask(t, sock, `{"cmd":"steer","job":"a","text":"go left"}`)
	if !resp.OK {
		t.Fatalf("steer = %+v", resp)
	}
	rep, ok := resp.Data.(map[string]any)
	if !ok || rep["frame_id"] != "steer-1" || rep["outcome"] != job.AckForwarded {
		t.Fatalf("steer data = %#v", resp.Data)
	}

	h.failOn["steer:b:go:false:false"] = errors.New("boom")
	resp = ask(t, sock, `{"cmd":"steer","job":"b","text":"go"}`)
	if resp.OK || resp.Error != "boom" {
		t.Fatalf("failing steer = %+v", resp)
	}
	if _, ok := resp.Data.(map[string]any); !ok {
		t.Fatalf("failed steer dropped its report: %#v", resp.Data)
	}
}

// Every dispatch branch: routing, the OK shape, and the error passthrough.
func TestDispatchRoutesEveryCommand(t *testing.T) {
	h := &watchHandler{fakeHandler: newFakeHandler()}
	h.logs = []string{"a", "b"}
	sock, stop := startServer(t, h)
	defer close(stop)

	cases := []struct {
		name    string
		raw     string
		wantOK  bool
		wantErr string
	}{
		{"status all", `{"cmd":"status"}`, true, ""},
		{"status one", `{"cmd":"status","job":"a"}`, true, ""},
		{"start", `{"cmd":"start","job":"a"}`, true, ""},
		{"stop", `{"cmd":"stop","job":"a"}`, true, ""},
		{"restart (no fresh)", `{"cmd":"restart","job":"a"}`, true, ""},
		{"restart (fresh)", `{"cmd":"restart","job":"a","fresh":true}`, true, ""},
		{"steer", `{"cmd":"steer","job":"a","text":"go left"}`, true, ""},
		{"logs", `{"cmd":"logs","job":"a","n":2}`, true, ""},
		{"logs default n", `{"cmd":"logs","job":"a"}`, true, ""},
		{"reload", `{"cmd":"reload"}`, true, ""},
		{"bad json", `{"cmd":`, false, "bad json"},
		{"unknown cmd", `{"cmd":"teleport"}`, false, "unknown cmd teleport"},
		{"empty cmd", `{}`, false, "unknown cmd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := ask(t, sock, tc.raw)
			if resp.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (err %q)", resp.OK, tc.wantOK, resp.Error)
			}
			if tc.wantErr != "" && !strings.Contains(resp.Error, tc.wantErr) {
				t.Fatalf("Error = %q, want it to contain %q", resp.Error, tc.wantErr)
			}
		})
	}

	// The handler actually saw each routed call. The three malformed/unknown
	// requests are rejected by dispatch itself and never reach the handler.
	want := []string{
		"statusall", "status:a", "start:a", "stop:a",
		"stop:a", "start:a", "restart:a", "steer:a:go left:false:false",
		"logs:a:2", "logs:a:0", "reload",
	}
	got := h.seen()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// The steer interrupt flag crosses the wire: request.interrupt becomes the
// handler's 4th argument, and the report echoes it back (ADR-0007).
func TestSteerInterruptReachesHandler(t *testing.T) {
	h := newFakeHandler()
	sock, stop := startServer(t, h)
	defer close(stop)

	resp := ask(t, sock, `{"cmd":"steer","job":"a","text":"stop","interrupt":true}`)
	if !resp.OK {
		t.Fatalf("steer interrupt = %+v", resp)
	}
	if got := h.seen(); len(got) != 1 || got[0] != "steer:a:stop:false:true" {
		t.Fatalf("handler saw %v, want [steer:a:stop:false:true]", got)
	}
	data, _ := resp.Data.(map[string]any)
	if iv, _ := data["interrupted"].(bool); !iv {
		t.Fatalf("report.interrupted = %v, want true (report %v)", data["interrupted"], data)
	}
}

// A steer without the flag leaves interrupt false and never signals.
func TestSteerWithoutInterruptFlagStaysFalse(t *testing.T) {
	h := newFakeHandler()
	sock, stop := startServer(t, h)
	defer close(stop)

	ask(t, sock, `{"cmd":"steer","job":"a","text":"note"}`)
	if got := h.seen(); len(got) != 1 || got[0] != "steer:a:note:false:false" {
		t.Fatalf("handler saw %v, want [steer:a:note:false:false]", got)
	}
}

// A handler error is surfaced as ok:false with the message, not a dropped
// connection.
func TestDispatchPropagatesHandlerErrors(t *testing.T) {
	h := newFakeHandler()
	boom := errors.New("boom")
	h.failOn["start:a"] = boom
	h.failOn["status:a"] = boom
	h.failOn["stop:a"] = boom
	h.failOn["restart:a"] = boom
	h.failOn["steer:a:x:false:false"] = boom
	h.failOn["logs:a:3"] = boom
	h.failOn["reload"] = boom
	sock, stop := startServer(t, h)
	defer close(stop)

	for _, raw := range []string{
		`{"cmd":"start","job":"a"}`,
		`{"cmd":"status","job":"a"}`,
		`{"cmd":"stop","job":"a"}`,
		`{"cmd":"restart","job":"a"}`,
		`{"cmd":"steer","job":"a","text":"x"}`,
		`{"cmd":"logs","job":"a","n":3}`,
		`{"cmd":"reload"}`,
	} {
		resp := ask(t, sock, raw)
		if resp.OK || resp.Error != "boom" {
			t.Fatalf("%s -> ok=%v err=%q, want ok=false err=boom", raw, resp.OK, resp.Error)
		}
	}
}

// A client that connects and closes without sending anything gets no reply
// and does not wedge the server (next request still answers).
func TestServeToleratesEmptyConnection(t *testing.T) {
	sock, stop := startServer(t, newFakeHandler())
	defer close(stop)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	if resp := ask(t, sock, `{"cmd":"reload"}`); !resp.OK {
		t.Fatalf("server wedged after an empty connection: %+v", resp)
	}
}

// A handler with no Watcher answers "watch unsupported" instead of blocking.
func TestWatchUnsupported(t *testing.T) {
	sock, stop := startServer(t, newFakeHandler()) // *fakeHandler has no Watch
	defer close(stop)

	resp := ask(t, sock, `{"cmd":"watch","job":"a"}`)
	if resp.OK || resp.Error != "watch unsupported" {
		t.Fatalf("got %+v, want ok=false watch unsupported", resp)
	}
}

// serveWatch: ack first, then filtered events, then close on terminal.
func TestServeWatchStreamsFilteredEvents(t *testing.T) {
	h := &watchHandler{fakeHandler: newFakeHandler()}
	sock, stop := startServer(t, h)
	defer close(stop)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Test client socket: a close error after the assertions cannot
	// fail the test, so it is deliberately ignored.
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte(`{"cmd":"watch","job":"a"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	var ack Response
	if err := json.Unmarshal(mustLine(t, r), &ack); err != nil {
		t.Fatal(err)
	}
	ackData, _ := ack.Data.(map[string]any)
	if !ack.OK || ackData["type"] != "watch_ack" || ackData["watching"] != "a" {
		t.Fatalf("first message = %+v, want a watch_ack for a", ack)
	}

	// Other jobs are filtered out; ours is delivered.
	h.events <- events.Event{Job: "b", Event: "round_done", Round: 9}
	h.events <- events.Event{Job: "a", Event: "round_done", Round: 1, RC: 0}
	var ev Response
	if err := json.Unmarshal(mustLine(t, r), &ev); err != nil {
		t.Fatal(err)
	}
	got, _ := ev.Data.(map[string]any)
	if got["job"] != "a" || got["event"] != "round_done" {
		t.Fatalf("event = %+v, want job a round_done (b must be filtered)", got)
	}

	// A terminal event ends the stream: the server closes without another line.
	h.events <- events.Event{Job: "a", Event: "done"}
	var term Response
	if err := json.Unmarshal(mustLine(t, r), &term); err != nil {
		t.Fatal(err)
	}
	td, _ := term.Data.(map[string]any)
	if td["event"] != "done" {
		t.Fatalf("terminal event = %+v, want done", td)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if line, err := r.ReadBytes('\n'); err == nil {
		t.Fatalf("server kept streaming after a terminal event: %q", string(line))
	}
}

// An empty job filter watches every job (label "*").
func TestServeWatchAllJobs(t *testing.T) {
	h := &watchHandler{fakeHandler: newFakeHandler()}
	sock, stop := startServer(t, h)
	defer close(stop)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Test client socket: a close error after the assertions cannot
	// fail the test, so it is deliberately ignored.
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte(`{"cmd":"watch"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ack Response
	if err := json.Unmarshal(mustLine(t, r), &ack); err != nil {
		t.Fatal(err)
	}
	ackData, _ := ack.Data.(map[string]any)
	if ackData["watching"] != "*" {
		t.Fatalf("watching = %v, want *", ackData["watching"])
	}
	h.events <- events.Event{Job: "whatever", Event: "ci_stall"}
	var ev Response
	if err := json.Unmarshal(mustLine(t, r), &ev); err != nil {
		t.Fatal(err)
	}
	got, _ := ev.Data.(map[string]any)
	if got["event"] != "ci_stall" {
		t.Fatalf("unfiltered watch dropped the event: %+v", got)
	}
}

// The precheck (already-terminal job) answers immediately and closes — no ack,
// no blocking.
func TestServeWatchPrecheckAnswersImmediately(t *testing.T) {
	h := &watchHandler{fakeHandler: newFakeHandler()}
	h.pre = &events.Event{Job: "a", Event: "done", Round: 3}
	sock, stop := startServer(t, h)
	defer close(stop)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Test client socket: a close error after the assertions cannot
	// fail the test, so it is deliberately ignored.
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte(`{"cmd":"watch","job":"a"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	var resp Response
	if err := json.Unmarshal(mustLine(t, r), &resp); err != nil {
		t.Fatal(err)
	}
	d, _ := resp.Data.(map[string]any)
	if !resp.OK || d["event"] != "done" {
		t.Fatalf("first message = %+v, want the precheck done event", resp)
	}
	if line, err := r.ReadBytes('\n'); err == nil {
		t.Fatalf("precheck kept the stream open: %q", string(line))
	}
}

// A disconnect cancels the subscription: the broker must not keep delivering
// to a dead watch (serveWatch's deferred cancel).
func TestServeWatchCancelsOnDisconnect(t *testing.T) {
	h := &watchHandler{fakeHandler: newFakeHandler()}
	sock, stop := startServer(t, h)
	defer close(stop)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(`{"cmd":"watch","job":"a"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_ = mustLine(t, r) // ack
	_ = c.Close()

	// Pumping events makes the server's writes fail, which is what drives it
	// through the deferred cancel.
	for range 4 {
		h.events <- events.Event{Job: "a", Event: "round_done"}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(fmt.Sprint(h.seen()), "cancel:a") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscription never canceled: calls %v", h.seen())
}

// Closing stop tears the listener down and Serve returns.
func TestServeStopsOnStopChannel(t *testing.T) {
	sock, stop := startServer(t, newFakeHandler())
	close(stop)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", sock)
		if err != nil {
			return
		}
		_ = c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Serve kept accepting after stop closed")
}

func mustLine(t *testing.T, r *bufio.Reader) []byte {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		t.Fatalf("read: %v", err)
	}
	return line
}
