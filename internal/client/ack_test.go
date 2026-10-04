package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-supervisor/internal/job"
)

// readAcks decodes the ack log a reader produced.
func readAcks(t *testing.T, path string) []job.AckRecord {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []job.AckRecord
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec job.AckRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad ack line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func onlyAck(t *testing.T, recs []job.AckRecord) job.AckRecord {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("acks = %+v, want exactly one", recs)
	}
	return recs[0]
}

// Every outcome `steer` can report is recorded by the client (ADR-0005).
func TestControlReaderRecordsDeliveryOutcomes(t *testing.T) {
	dir := t.TempDir()
	ctrl, ack := filepath.Join(dir, "job.ctrl"), filepath.Join(dir, "job_ack.jsonl")

	t.Run("forwarded", func(t *testing.T) {
		c := newReaderWithAck(t, ctrl, ack)
		rec := &sent{}
		var diag strings.Builder
		appendTo(t, ctrl, `{"id":"s1","type":"prompt","message":"go"}`+"\n")
		c.pump(rec.send, &stream{}, &diag)
		if len(rec.frames()) != 1 {
			t.Fatalf("frame not forwarded: %v", rec.frames())
		}
		got := onlyAck(t, readAcks(t, ack))
		if got.Outcome != job.AckForwarded || got.ID != "s1" || got.Type != "prompt" {
			t.Fatalf("ack = %+v, want forwarded for s1", got)
		}
		if !got.Terminal() {
			t.Fatal("forwarded must be terminal")
		}
		_ = os.Remove(ack)
	})

	t.Run("bad frame", func(t *testing.T) {
		c := newReaderWithAck(t, ctrl, ack)
		rec := &sent{}
		var diag strings.Builder
		appendTo(t, ctrl, "this is prose, not a frame\n")
		c.pump(rec.send, &stream{}, &diag)
		got := onlyAck(t, readAcks(t, ack))
		if got.Outcome != job.AckBadFrame {
			t.Fatalf("ack = %+v, want bad frame", got)
		}
		if !strings.Contains(got.Detail, "prose") {
			t.Fatalf("detail = %q, want the dropped line", got.Detail)
		}
		_ = os.Remove(ack)
	})

	t.Run("send failed", func(t *testing.T) {
		c := newReaderWithAck(t, ctrl, ack)
		var diag strings.Builder
		boom := errBoom("stdin closed")
		appendTo(t, ctrl, `{"id":"s2","type":"prompt","message":"go"}`+"\n")
		c.pump(func(any) error { return boom }, &stream{}, &diag)
		got := onlyAck(t, readAcks(t, ack))
		if got.Outcome != job.AckSendFail || got.ID != "s2" || got.Detail == "" {
			t.Fatalf("ack = %+v, want send failed for s2", got)
		}
		_ = os.Remove(ack)
	})

	t.Run("held then delivered", func(t *testing.T) {
		c := newReaderWithAck(t, ctrl, ack)
		rec := &sent{}
		var diag strings.Builder
		// An abort first opens the drain; the steer behind it must be held
		// and acked as such, then delivered with the wait measured.
		appendTo(t, ctrl, `{"id":"a1","type":"abort"}`+"\n"+
			`{"id":"s3","type":"prompt","message":"go"}`+"\n")
		st := &stream{}
		c.pump(rec.send, st, &diag)
		recs := readAcks(t, ack)
		if len(recs) != 2 || recs[1].Outcome != job.AckHeld || recs[1].ID != "s3" {
			t.Fatalf("acks = %+v, want the steer held", recs)
		}
		if recs[1].Terminal() {
			t.Fatal("held must not be terminal: the frame has not landed yet")
		}
		if len(rec.frames()) != 1 || rec.frames()[0]["id"] != "a1" {
			t.Fatalf("frames = %v, want only the abort sent now", rec.frames())
		}
		// The aborted turn ends; the next pump releases the drain.
		st.mu.Lock()
		st.turnEnd = true
		st.mu.Unlock()
		c.pump(rec.send, st, &diag)
		recs = readAcks(t, ack)
		last := recs[len(recs)-1]
		if last.ID != "s3" || last.Outcome != job.AckForwarded || !last.Terminal() {
			t.Fatalf("terminal ack = %+v, want s3 forwarded", last)
		}
		if last.DelayMS < 0 || !strings.Contains(last.Detail, "abort") {
			t.Fatalf("terminal ack = %+v, want the hold explained", last)
		}
		if len(rec.frames()) != 2 || rec.frames()[1]["id"] != "s3" {
			t.Fatalf("frames = %v, want the held steer delivered", rec.frames())
		}
	})
}

type errBoom string

func (e errBoom) Error() string { return string(e) }

// An unopenable ack path must not break steering: the frame is still
// forwarded and the diagnostic still says so.
func TestControlReaderSurvivesUnusableAckPath(t *testing.T) {
	dir := t.TempDir()
	ctrl := filepath.Join(dir, "job.ctrl")
	if err := os.WriteFile(ctrl, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := newControlReader(ctrl, dir, func(string, ...any) {}, 0) // ack path is a directory
	rec := &sent{}
	var diag strings.Builder
	appendTo(t, ctrl, `{"id":"s4","type":"prompt","message":"go"}`+"\n")
	c.pump(rec.send, &stream{}, &diag)
	if len(rec.frames()) != 1 {
		t.Fatalf("frame not forwarded: %v", rec.frames())
	}
	if !strings.Contains(diag.String(), "forwarded") {
		t.Fatalf("diag = %q", diag.String())
	}
}

// The reader starts at the beginning of the file: the supervisor truncates
// the ctrl file at every round start, so a steer written between the
// truncate and the spawn must not be dropped.
func TestControlReaderReadsWholeRoundFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ctrl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, `{"id":"early","type":"prompt","message":"go"}`+"\n")
	c := newControlReader(path, "", func(string, ...any) {}, 0)
	rec := &sent{}
	var diag strings.Builder
	c.pump(rec.send, &stream{}, &diag)
	if got := rec.frames(); len(got) != 1 || got[0]["id"] != "early" {
		t.Fatalf("frames = %v, want the pre-spawn steer", got)
	}
}
