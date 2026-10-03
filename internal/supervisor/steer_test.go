package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// fastControlPoll shrinks the client's control-file poll period so a steer is
// delivered and acked in milliseconds instead of after the 5s default.
func fastControlPoll(t *testing.T, d time.Duration) {
	t.Helper()
	old := controlPollInterval
	controlPollInterval = d
	t.Cleanup(func() { controlPollInterval = old })
}

// armFakeRound marks a loaded job's runner as running without a real pi, so
// the ack-wait paths can be exercised deterministically.
func armFakeRound(t *testing.T, s *Supervisor, name string, pid int) {
	t.Helper()
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		t.Fatalf("job %q not loaded", name)
	}
	r.mu.Lock()
	r.state.State, r.state.Round, r.active, r.pid = "running", 1, true, pid
	r.mu.Unlock()
}

func disarmRound(s *Supervisor, name string) {
	s.mu.Lock()
	r := s.jobs[name]
	s.mu.Unlock()
	r.mu.Lock()
	r.active, r.pid = false, 0
	r.mu.Unlock()
}

// appendAck plays the client's part: one more ack record for a frame id.
func appendAck(t *testing.T, path string, rec job.AckRecord) {
	t.Helper()
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// waitAckVisible blocks until the ack log at path contains a record with the
// given id and outcome, as read through acksSince — the exact path waitAck
// uses — so the helper synchronizes with the waiter's view instead of racing
// it on a blind timer. Fails the test on timeout (default 5s).
func waitAckVisible(t *testing.T, path, wantID, wantOutcome string) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("ack %s/%s never observed in %s", wantOutcome, wantID, path)
		case <-tick.C:
		}
		// acksSince opens the file from offset 0; that matches how waitAck
		// sweeps on each poll.
		for _, rec := range acksSince(path, 0) {
			if rec.ID == wantID && rec.Outcome == wantOutcome {
				return
			}
		}
	}
}

// lastFrameID reads the newest control-frame id from a ctrl file. It returns
// ("", false) if the file is absent, empty, or holds no well-formed frame, so
// a test helper can poll without distinguishing "not yet" from "broken".
func lastFrameID(t *testing.T, path string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	id := ""
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var f map[string]any
		if json.Unmarshal([]byte(ln), &f) == nil {
			if v, ok := f["id"].(string); ok && v != "" {
				id = v
			}
		}
	}
	return id, id != ""
}

// The documented usage is prose: `steer <job> 'POLICY CHANGE ...'`. It must
// arrive at pi as a real prompt frame, and the CLI must be told it landed.
func TestSteerProseReachesPiStdin(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)
	fastControlPoll(t, 150*time.Millisecond)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	// TEST_FRAMELOG keeps the round live and quiet so the steer has a round
	// to land in.
	if err := os.WriteFile(brief, []byte("TEST_FRAMELOG"), 0o644); err != nil {
		t.Fatal(err)
	}
	frameLog := filepath.Join(dir, "frames.jsonl")
	t.Setenv("FAKE_PI_FRAME_LOG", frameLog)

	j := job.Job{
		Name: "prose", Brief: brief, Worktree: t.TempDir(), SessionName: "prose",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	s.steerWait = 15 * time.Second
	if err := s.Start("prose"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool {
		st, _ := s.Status("prose")
		m, _ := st.(job.Status)
		return m.ClientPID != 0
	})

	rep, err := s.Steer("prose", "TEST_FRAMELOG POLICY CHANGE FROM THE OWNER: stop the review loop", false, false)
	if err != nil {
		t.Fatalf("Steer: %v (report %+v)", err, rep)
	}
	if rep.Outcome != job.AckForwarded || !rep.Confirmed {
		t.Fatalf("outcome = %q confirmed=%v detail=%q, want forwarded", rep.Outcome, rep.Confirmed, rep.Detail)
	}
	if rep.FrameID == "" || !rep.LiveRound || rep.Round != 1 || rep.ClientPID == 0 {
		t.Fatalf("report does not say where it went: %+v", rep)
	}
	if rep.CtrlPath != job.Ctrl("prose") || rep.AckPath != job.Ack("prose") {
		t.Fatalf("report paths = %q/%q", rep.CtrlPath, rep.AckPath)
	}
	if !strings.HasSuffix(rep.SessionPath, "session.jsonl") {
		t.Fatalf("session path = %q", rep.SessionPath)
	}

	// The prose reached pi's stdin as a prompt frame with our id.
	waitFor(t, 15*time.Second, func() bool {
		raw, err := os.ReadFile(frameLog)
		if err != nil {
			return false
		}
		return strings.Contains(string(raw), "POLICY CHANGE FROM THE OWNER")
	})
	raw, err := os.ReadFile(frameLog)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var f map[string]any
		if json.Unmarshal([]byte(line), &f) != nil {
			continue
		}
		if f["id"] == rep.FrameID {
			found = true
			if f["type"] != "prompt" {
				t.Fatalf("steered frame type = %v, want prompt", f["type"])
			}
			if msg, _ := f["message"].(string); !strings.Contains(msg, "POLICY CHANGE FROM THE OWNER") {
				t.Fatalf("steered message = %q", msg)
			}
		}
	}
	if !found {
		t.Fatalf("frame id %q never reached pi's stdin; log:\n%s", rep.FrameID, raw)
	}
}

// An idle job has nothing reading the ctrl file and round() truncates it at
// the start of every round: queueing there would silently lose the steer.
func TestSteerNoLiveRoundIsNotQueued(t *testing.T) {
	testEnv(t)
	writeJob(t, job.Job{
		Name: "idle", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "idle", MaxRounds: 1, TimeoutS: 20, PiBin: "true",
	})
	s := newTestSupervisor(t)

	rep, err := s.Steer("idle", "POLICY CHANGE", false, false)
	if err == nil {
		t.Fatalf("Steer on an idle job must fail, got %+v", rep)
	}
	if rep.Outcome != job.AckNoRound || rep.Confirmed {
		t.Fatalf("outcome = %q confirmed=%v, want no live round", rep.Outcome, rep.Confirmed)
	}
	if rep.FrameID == "" || rep.LiveRound {
		t.Fatalf("report = %+v, want an identified frame with live_round=false", rep)
	}
	if job.Size(rep.CtrlPath) != 0 {
		t.Fatalf("ctrl file grew to %d bytes; the frame would only be wiped", job.Size(rep.CtrlPath))
	}
}

// A hand-written JSON frame keeps its type and id (abort frames, custom
// frames); prose is the only thing that gets wrapped.
func TestSteerPassesThroughJSONFrame(t *testing.T) {
	testEnv(t)
	writeJob(t, job.Job{
		Name: "pass", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "pass", MaxRounds: 1, TimeoutS: 20, PiBin: "true",
	})
	s := newTestSupervisor(t)
	armFakeRound(t, s, "pass", 4242)

	rep, err := s.Steer("pass", `{"id":"my-own-id","type":"abort"}`, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FrameID != "my-own-id" || rep.Outcome != job.AckWritten {
		t.Fatalf("report = %+v", rep)
	}
	data, err := os.ReadFile(job.Ctrl("pass"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `{"id":"my-own-id","type":"abort"}`) {
		t.Fatalf("ctrl file = %q, want the frame untouched", data)
	}

	// A JSON object that is not a frame (no type) is prose, not garbage.
	rep, err = s.Steer("pass", `{"just":"data"}`, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rep.FrameID, "steer-") {
		t.Fatalf("frame id = %q, want a generated one", rep.FrameID)
	}
}

// Held is not delivered: `steer` must keep waiting and report the terminal
// record, including how long the frame waited behind the abort.
func TestSteerReportsHeldThenDelivered(t *testing.T) {
	testEnv(t)
	// Unique job name per test ITERATION so job.Ack/Ctrl's fixed
	// /tmp/pi_<name>_* paths do not collide across -count runs (t.Name()
	// is identical for every iteration of the same test). Without this,
	// run 2's waiter reads run 1's stale Held record and the held-then-
	// delivered detail never fires.
	name := "held-" + t.Name() + fmt.Sprintf("-run%d", time.Now().UnixNano())
	writeJob(t, job.Job{
		Name: name, Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: name, MaxRounds: 1, TimeoutS: 20, PiBin: "true",
	})
	s := newTestSupervisor(t)
	s.steerWait = 20 * time.Second
	armFakeRound(t, s, name, 4343)

	go func() {
		// Play held -> forwarded. We do NOT discover the frame id by polling the
		// ctrl file with a deadline (the prior version did: a 15s discovery
		// window that lost the race under a parallel `go test -race` run,
		// because the scheduler can starve this goroutine for seconds while
		// Steer blocks its caller). Instead we append ack records for ANY id
		// we see in the ctrl file: Steer's waitAck filters by FrameID, so a
		// record whose id has not landed yet is simply ignored and re-scanned
		// on the next poll. The loop runs until the waiter has gone quiet, so
		// there is no discovery deadline to lose and no fixed sleeps to misjudge.
		heldWritten := false
		// Run no longer than the supervisor's own steerWait budget so the test
		// fails fast instead of waiting for the 20s timeout when something is
		// actually wrong.
		deadline := time.After(s.steerWait)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline:
				return
			case <-ticker.C:
			}
			id, ok := lastFrameID(t, job.Ctrl(name))
			if !ok || id == "" {
				continue
			}
			if !heldWritten {
				appendAck(t, job.Ack(name), job.AckRecord{
					ID: id, Outcome: job.AckHeld, Type: "prompt",
				})
				heldWritten = true
				// waitAck only tags detail as "held then delivered" when it
				// observes the Held record BEFORE the Forwarded one in the same
				// poll batch. A blind sleep cannot guarantee that under `go test
				// -race` (the scheduler can deliver both acks before the 100ms
				// waiter poll fires). Synchronize instead: block until the held
				// ack is actually visible through the same acksSince path the
				// waiter reads, then write forwarded. This makes the
				// two-step transition deterministic, not timing-dependent.
				waitAckVisible(t, job.Ack(name), id, job.AckHeld)
				continue
			}
			appendAck(t, job.Ack(name), job.AckRecord{
				ID:      id,
				Outcome: job.AckForwarded,
				Type:    "prompt",
				Detail:  "queued behind an abort; delivered once the drain settled",
				DelayMS: 320,
			})
		}
	}()

	rep, err := s.Steer(name, "POLICY CHANGE", false, false)
	if err != nil {
		t.Fatalf("Steer: %v (%+v)", err, rep)
	}
	if rep.Outcome != job.AckForwarded || !rep.Confirmed {
		t.Fatalf("outcome = %q confirmed=%v, want forwarded", rep.Outcome, rep.Confirmed)
	}
	if rep.DelayMS != 320 {
		t.Fatalf("delay = %dms, want the client's 320ms", rep.DelayMS)
	}
	if !strings.Contains(rep.Detail, "accepted while an abort drained") {
		t.Fatalf("detail = %q, want it to say the frame was held then delivered", rep.Detail)
	}
}

// Silence is not success: a round that never acks reports "not confirmed"
// and exits non-zero, and a round that dies mid-wait says so.
func TestSteerUnconfirmedIsNotReportedAsSuccess(t *testing.T) {
	testEnv(t)
	writeJob(t, job.Job{
		Name: "quiet", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "quiet", MaxRounds: 1, TimeoutS: 20, PiBin: "true",
	})
	s := newTestSupervisor(t)
	s.steerWait = 600 * time.Millisecond
	armFakeRound(t, s, "quiet", 4444)

	rep, err := s.Steer("quiet", "POLICY CHANGE", false, false)
	if err == nil {
		t.Fatal("an unacked steer must be an error, not a success")
	}
	if rep.Outcome != job.AckUnconfirmed || rep.Confirmed {
		t.Fatalf("outcome = %q confirmed=%v", rep.Outcome, rep.Confirmed)
	}
	if !strings.Contains(rep.Detail, "unconfirmed") {
		t.Fatalf("detail = %q", rep.Detail)
	}

	// Same, but the round dies while we wait: say the frame was not
	// delivered instead of leaving the operator guessing.
	armFakeRound(t, s, "quiet", 4545)
	go func() {
		time.Sleep(150 * time.Millisecond)
		disarmRound(s, "quiet")
	}()
	rep, err = s.Steer("quiet", "POLICY CHANGE AGAIN", false, false)
	if err == nil {
		t.Fatal("an unacked steer must be an error")
	}
	if rep.Outcome != job.AckUnconfirmed || rep.Confirmed {
		t.Fatalf("outcome = %q confirmed=%v", rep.Outcome, rep.Confirmed)
	}
	if !strings.Contains(rep.Detail, "truncates") {
		t.Fatalf("detail = %q, want the round-ended explanation", rep.Detail)
	}
}

// Empty text is rejected before anything is written.
func TestSteerRejectsEmptyText(t *testing.T) {
	testEnv(t)
	writeJob(t, job.Job{
		Name: "empty", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "empty", MaxRounds: 1, TimeoutS: 20, PiBin: "true",
	})
	s := newTestSupervisor(t)
	armFakeRound(t, s, "empty", 4646)
	if _, err := s.Steer("empty", "   \n", true, false); err == nil {
		t.Fatal("empty steer text must be rejected")
	}
	if job.Size(job.Ctrl("empty")) != 0 {
		t.Fatal("empty steer wrote to the ctrl file")
	}
}
