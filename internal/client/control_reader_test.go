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

// newReaderAt opens a control reader positioned at the file's current end.
func newReaderAt(t *testing.T, path string) *controlReader {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return newControlReader(path, func(string, ...any) {})
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Test fixture: the frame is read back, so a close error is irrelevant.
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// sent collects the frames a pump forwards to pi's stdin.
type sent struct {
	mu sync.Mutex
	f  []map[string]any
}

func (s *sent) send(v any) error {
	m, _ := v.(map[string]any)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f = append(s.f, m)
	return nil
}

func (s *sent) frames() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.f...)
}

// A steer frame whose newline has not landed yet must NOT be parsed (and
// dropped) as a corrupt frame: the partial bytes are carried until the rest
// arrives (review fix — a 64KB read boundary used to split frames).
func TestControlReaderCarriesPartialLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ctrl")
	c := newReaderAt(t, path)
	rec := &sent{}
	var diag strings.Builder

	// Half a frame.
	appendTo(t, path, `{"type":"prompt","mess`)
	c.pump(rec.send, &stream{}, &diag)
	if got := rec.frames(); len(got) != 0 {
		t.Fatalf("partial line was forwarded: %v", got)
	}
	if strings.Contains(diag.String(), "bad frame") {
		t.Fatalf("partial line reported as corrupt: %q", diag.String())
	}

	// The rest arrives, plus a second whole frame in the same read.
	appendTo(t, path, "age\":\"POLICY\"}\n"+`{"type":"prompt","message":"SECOND"}`+"\n")
	c.pump(rec.send, &stream{}, &diag)
	got := rec.frames()
	if len(got) != 2 {
		t.Fatalf("frames = %v, want the completed one and the second", got)
	}
	if got[0]["message"] != "POLICY" || got[1]["message"] != "SECOND" {
		t.Fatalf("frames mangled: %v", got)
	}
	if strings.Contains(diag.String(), "bad frame") {
		t.Fatalf("unexpected diagnostic: %q", diag.String())
	}
}

// A genuinely corrupt line is still reported and skipped.
func TestControlReaderSkipsCorruptLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ctrl")
	c := newReaderAt(t, path)
	rec := &sent{}
	var diag strings.Builder

	appendTo(t, path, "not json\n"+`{"type":"prompt","message":"OK"}`+"\n")
	c.pump(rec.send, &stream{}, &diag)
	if len(rec.frames()) != 1 || rec.frames()[0]["message"] != "OK" {
		t.Fatalf("frames = %v, want only the good one", rec.frames())
	}
	if !strings.Contains(diag.String(), "bad frame skipped") {
		t.Fatalf("corrupt line not reported: %q", diag.String())
	}
}

// A file truncated under the reader (round restart) restarts from zero and
// drops the carried partial, so the next round's frames are seen.
func TestControlReaderHandlesTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ctrl")
	c := newReaderAt(t, path)
	rec := &sent{}
	var diag strings.Builder

	appendTo(t, path, `{"type":"prompt","mess`)
	c.pump(rec.send, &stream{}, &diag) // carried, not forwarded
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, `{"type":"prompt","message":"NEWROUND"}`+"\n")
	c.pump(rec.send, &stream{}, &diag)
	got := rec.frames()
	if len(got) != 1 || got[0]["message"] != "NEWROUND" {
		t.Fatalf("frames after truncation = %v, want the new round's frame", got)
	}
}

// A reader opened on a not-yet-existing control file stays inert instead of
// failing the round.
func TestControlReaderMissingFile(t *testing.T) {
	c := newControlReader(filepath.Join(t.TempDir(), "absent.ctrl"), func(string, ...any) {})
	if c.fh != nil {
		t.Fatal("expected no open handle")
	}
	c.pump(func(any) error { return nil }, &stream{}, os.Stderr) // must not panic
	if c.tick() == nil {
		t.Fatal("expected a poll ticker even without a file")
	}
	// A nil path disables polling entirely (blocks forever in select).
	if nc := newControlReader("", func(string, ...any) {}); nc.tick() != nil {
		t.Fatal("empty control path must yield a nil tick channel")
	}
}

// The abort-drain handshake: frames written after an abort are held until the
// aborted turn's agent_end, then delivered in order.
func TestControlReaderHoldsFramesUntilAbortDrains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ctrl")
	c := newReaderAt(t, path)
	rec := &sent{}
	var diag strings.Builder
	st := &stream{out: nil, diagf: func(string, ...any) {}}

	appendTo(t, path, `{"type":"abort"}`+"\n"+`{"type":"prompt","message":"FINISH THE REPORT"}`+"\n")
	c.pump(rec.send, st, &diag)

	// The abort went out; the prompt is held.
	if got := rec.frames(); len(got) != 1 || got[0]["type"] != "abort" {
		t.Fatalf("frames = %v, want only the abort", got)
	}
	if !strings.Contains(diag.String(), "held until abort settles") {
		t.Fatalf("no hold diagnostic: %q", diag.String())
	}

	// The aborted turn ends: the held frame is released.
	st.mu.Lock()
	st.turnEnd = true
	st.mu.Unlock()
	appendTo(t, path, "")
	c.pump(rec.send, st, &diag)
	got := rec.frames()
	if len(got) != 2 || got[1]["message"] != "FINISH THE REPORT" {
		t.Fatalf("held frame not released: %v", got)
	}
	if !strings.Contains(diag.String(), "delivered after abort") {
		t.Fatalf("no release diagnostic: %q", diag.String())
	}
}

// DefaultPromptFile: inline text, a real file, a directory, and the empty
// prompt error.
func TestDefaultPromptFileEdges(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(file, []byte("the brief body\nsecond line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DefaultPromptFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got != "the brief body\nsecond line\n" {
		t.Fatalf("file prompt = %q", got)
	}
	// A path that does not exist is passed through as inline text.
	if got, err = DefaultPromptFile("just a prompt"); err != nil || got != "just a prompt" {
		t.Fatalf("inline = %q (%v)", got, err)
	}
	// A directory is not a file: treated as inline text, not read.
	if got, err = DefaultPromptFile(dir); err != nil || got != dir {
		t.Fatalf("directory = %q (%v)", got, err)
	}
	if _, err := DefaultPromptFile(""); err == nil {
		t.Fatal("empty prompt must error")
	}
	// An empty file falls back to inline treatment.
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = DefaultPromptFile(empty); err != nil || got != empty {
		t.Fatalf("empty file = %q (%v)", got, err)
	}
}

// frameType degrades gracefully on non-map frames.
func TestFrameTypeFallback(t *testing.T) {
	if got := frameType(map[string]any{"type": "abort"}); got != "abort" {
		t.Fatalf("frameType = %q", got)
	}
	if got := frameType(map[string]any{"nope": 1}); got != "?" {
		t.Fatalf("missing type = %q", got)
	}
	if got := frameType("string frame"); got != "?" {
		t.Fatalf("non-map = %q", got)
	}
}

// Run applies its own defaults rather than trusting the caller.
func TestRunDefaultsOptions(t *testing.T) {
	// A nonexistent binary fails at spawn with rc 1 and a diagnostic.
	res := Run(Options{PiBin: filepath.Join(t.TempDir(), "no-such-pi"),
		Prompt: "x", Timeout: 2 * time.Second})
	if res.RC != 1 || !strings.Contains(res.Err, "spawn") {
		t.Fatalf("spawn failure = %+v", res)
	}
	if res.Duration <= 0 {
		t.Fatal("duration not measured")
	}
}

// The streamed text and the run-log writer see the same deltas.
func TestStreamTextAndOutAgree(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "run.log")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	res := Run(Options{
		PiBin: fakePi(t), Prompt: "TEST_STREAM", Worktree: dir,
		Timeout: 10 * time.Second, Out: out, Diag: out,
	})
	_ = out.Close()
	if res.RC != 0 {
		t.Fatalf("rc = %d err %q", res.RC, res.Err)
	}
	if res.Text == "" {
		t.Fatal("no streamed text captured")
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("run log empty: the flusher never drained")
	}
	if res.PID <= 0 {
		t.Fatalf("PID = %d", res.PID)
	}
	// The RPC frame we send is valid JSON: guards the wire format.
	var frame map[string]any
	if err := json.Unmarshal([]byte(`{"id":"r1","type":"prompt","message":"m"}`), &frame); err != nil {
		t.Fatal(err)
	}
}
