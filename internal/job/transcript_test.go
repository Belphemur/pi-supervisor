package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reproduces the mealime-roomux bug (ADR-0011): the marker gate read the
// runlog, which round() TRUNCATES at the start of every round. A marker that
// pi emitted in an earlier round was structurally invisible, so a finished run
// burned MaxRounds and ended fatal with "round cap reached without marker".
//
// Every fixture below uses the REAL persisted-session record shapes (see
// transcript.go): `{"type":"message","message":{"role":...,"content":[...]}}`
// plus `{"type":"custom","customType":"dcp-state",...}`. There are no
// text_delta records in a persisted session log.

// writeTL TRUNCATES and writes (used for initial fixtures).
func writeTL(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// appendTL APPENDS, as a live transcript grows. A truncating writer here would
// be a lie: the watcher is built for an append-only file.
func appendTL(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func asstText(text string) string {
	return `{"type":"message","id":"x","message":{"role":"assistant","content":[{"type":"text","text":` +
		mustJSON(text) + `}]}}`
}

func mustJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// The core bug: the marker rode in an assistant message in the transcript,
// while the runlog for that round is long gone. It must be found.
func TestTranscriptContainsFindsMarkerInAssistantText(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	writeTL(t, sess,
		`{"type":"session","id":"a"}`,
		asstText("work is committed and pushed"),
		asstText("The phase is complete.\n\n## ALL_MEALIME_ROOMUX_DONE\n**pr https://github.com/Belphemur/flambette/pull/43**"),
	)
	if !TranscriptContains(sess, "ALL_MEALIME_ROOMUX_DONE") {
		t.Fatal("marker in an assistant text block was not found")
	}
}

// THE FALSE-POSITIVE TRAP, and the reason scanning whole records is wrong:
// DCP compression summaries QUOTE the marker back (27 such records in the real
// file), and so do toolCall arguments. A completion gate must ignore those, or
// it declares done on a compressed prompt that merely mentions the token.
func TestTranscriptContainsIgnoresDcpAndNonAssistantEchoes(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	writeTL(t, sess,
		// DCP compression summary quoting the marker out of the brief.
		`{"type":"custom","customType":"dcp-state","data":{"compressionBlocks":[{"id":1,"summary":"MISSION: emit ALL_MEALIME_ROOMUX_DONE when finished"}]}}`,
		// The USER brief instructs the agent to print the marker. An assistant
		// that has not finished yet still has this in context.
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Finish with ALL_MEALIME_ROOMUX_DONE"}]}}`,
		// A bash tool call that echoed the token.
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"printf ALL_MEALIME_ROOMUX_DONE"}}]}}`,
		// The agent is still working — no real completion text.
		asstText("Still exploring the room controller."),
	)
	if TranscriptContains(sess, "ALL_MEALIME_ROOMUX_DONE") {
		t.Fatal("a DCP/user/toolCall echo was mistaken for completion")
	}

	// And it flips to true only on genuine assistant text.
	writeTL(t, sess,
		`{"type":"custom","customType":"dcp-state","data":{"compressionBlocks":[{"id":1,"summary":"ALL_MEALIME_ROOMUX_DONE"}]}}`,
		asstText("Done. ALL_MEALIME_ROOMUX_DONE"),
	)
	if !TranscriptContains(sess, "ALL_MEALIME_ROOMUX_DONE") {
		t.Fatal("genuine assistant completion text was not found")
	}
}

// Content as a bare string (not a block array) is also a valid assistant shape.
func TestTranscriptContainsHandlesStringContent(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	writeTL(t, sess, `{"type":"message","message":{"role":"assistant","content":"ALL_MEALIME_ROOMUX_DONE"}}`)
	if !TranscriptContains(sess, "ALL_MEALIME_ROOMUX_DONE") {
		t.Fatal("string-shaped assistant content was not found")
	}
}

// The real failure burned 14 rounds because the marker kept being re-emitted
// in later rounds' final messages while the gate looked at a truncated runlog.
// Many occurrences across many rounds must all be found.
func TestTranscriptContainsFindsMarkerReEmittedEveryRound(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	var lines []string
	for i := 0; i < 14; i++ {
		lines = append(lines,
			asstText("round work"),
			asstText("still complete: ALL_MEALIME_ROOMUX_DONE"),
		)
	}
	writeTL(t, sess, lines...)
	if !TranscriptContains(sess, "ALL_MEALIME_ROOMUX_DONE") {
		t.Fatal("marker re-emitted across rounds was not found")
	}
}

// A torn trailing record (pi mid-write) must not lose a marker already present
// in a complete earlier record.
func TestTranscriptContainsIgnoresTornTail(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	body := asstText("ALL_MEALIME_ROOMUX_DONE") + "\n"
	if err := os.WriteFile(sess, []byte(body+`{"type":"message","message":{"role":"assis`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !TranscriptContains(sess, "ALL_MEALIME_ROOMUX_DONE") {
		t.Fatal("marker in a complete record was lost to a torn tail")
	}
}

func TestTranscriptContainsMissingFile(t *testing.T) {
	if TranscriptContains(filepath.Join(t.TempDir(), "nope.jsonl"), "X") {
		t.Fatal("missing file reported a match")
	}
}

// An empty marker must never match, or every finished job is instantly done.
func TestTranscriptContainsEmptyMarkerNeverMatches(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	writeTL(t, sess, asstText("anything at all"))
	if TranscriptContains(sess, "") {
		t.Fatal("empty marker matched — this would declare every job done")
	}
}

// Cost must stay flat on a multi-MB transcript (the real one was 1.7MB): the
// scan reads at most the trailing window, never the whole file.
func TestTranscriptContainsIsWindowBounded(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	var b strings.Builder
	for b.Len() < 4*1024*1024 {
		b.WriteString(asstText(strings.Repeat("y", 900)) + "\n")
	}
	b.WriteString(asstText("ALL_AT_THE_END_DONE") + "\n")
	if err := os.WriteFile(sess, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if !TranscriptContains(sess, "ALL_AT_THE_END_DONE") {
		t.Fatal("marker at the end of a 4MB transcript was not found")
	}
}

// --- streaming watcher -----------------------------------------------------

// The watcher must detect the marker in bytes appended AFTER it started: this
// is the "stream and keep parsing as we stream" path, and the one the runlog
// could not provide at all.
func TestTranscriptWatcherLatchesOnAppend(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	writeTL(t, sess, `{"type":"session","id":"a"}`)

	w := NewTranscriptWatcher(sess, "ALL_MEALIME_ROOMUX_DONE", 0)
	if w.Seen() {
		t.Fatal("latched before anything was written")
	}
	// Work in progress: no marker.
	appendTL(t, sess, asstText("thinking about the room controller"))
	if w.Poll() {
		t.Fatal("latched on a non-completion message")
	}
	// pi finishes and emits the marker.
	appendTL(t, sess, asstText("complete: ALL_MEALIME_ROOMUX_DONE"))
	if !w.Poll() {
		t.Fatal("did not latch on the appended marker")
	}
	if !w.Seen() {
		t.Fatal("Seen() disagrees with Poll()")
	}
}

// The latch is sticky and does no further I/O: a later Poll must stay true even
// if the file is gone. This is the property that breaks the livelock.
func TestTranscriptWatcherLatchIsSticky(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	writeTL(t, sess, `{"type":"session"}`)
	w := NewTranscriptWatcher(sess, "MK", 0)
	appendTL(t, sess, asstText("MK"))
	if !w.Poll() {
		t.Fatal("did not latch")
	}
	if err := os.Remove(sess); err != nil {
		t.Fatal(err)
	}
	if !w.Poll() {
		t.Fatal("latch cleared after the file disappeared")
	}
}

// A watcher must not fire on a DCP echo arriving mid-stream.
func TestTranscriptWatcherIgnoresDcpEcho(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	writeTL(t, sess, `{"type":"session"}`)
	w := NewTranscriptWatcher(sess, "MK", 0)
	appendTL(t, sess, `{"type":"custom","customType":"dcp-state","data":{"summary":"MK"}}`)
	if w.Poll() {
		t.Fatal("latched on a DCP summary echo")
	}
}

// Repeated polling while idle must be cheap and never lose its place: the
// offset advances monotonically so bytes are consumed exactly once.
func TestTranscriptWatcherConsumesEachByteOnce(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	writeTL(t, sess, `{"type":"session"}`)
	w := NewTranscriptWatcher(sess, "NEVER_EMITTED", 0)

	prev := w.Offset()
	for i := 0; i < 5; i++ {
		appendTL(t, sess, asstText(strings.Repeat("z", 200)))
		w.Poll()
		if w.Offset() < prev {
			t.Fatalf("offset went backwards: %d < %d", w.Offset(), prev)
		}
		prev = w.Offset()
	}
	// The marker arrives at the very end and is still found.
	appendTL(t, sess, asstText("NEVER_EMITTED"))
	if !w.Poll() {
		t.Fatal("marker at the end of an incrementally-grown file was not found")
	}
}

// A watcher over a missing file must be inert, not panic, until the file
// appears (a fresh LAUNCH writes the transcript only after the session starts).
func TestTranscriptWatcherMissingFileThenAppears(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	w := NewTranscriptWatcher(sess, "MK", 0)
	if w.Poll() {
		t.Fatal("latched on a missing file")
	}
	appendTL(t, sess, asstText("MK"))
	if !w.Poll() {
		t.Fatal("did not latch once the transcript appeared")
	}
}

// A watcher with an empty marker must never latch (the safety invariant from
// the done gate, enforced at the primitive).
func TestTranscriptWatcherEmptyMarkerNeverLatches(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	writeTL(t, sess, `{"type":"session"}`)
	w := NewTranscriptWatcher(sess, "", 0)
	appendTL(t, sess, asstText("anything"))
	if w.Poll() {
		t.Fatal("empty marker latched")
	}
}
