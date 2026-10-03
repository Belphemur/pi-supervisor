package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// interruptProbe is a stand-in for pi: a real process, placed in its OWN
// process group, that records the SIGINT it receives by creating a marker file.
// Using a real process (not a mocked pid) is the point — it proves the signal
// reaches the group the way it reaches pi, which the whole feature rests on.
func interruptProbe(t *testing.T, marker string) int {
	t.Helper()
	// sh traps INT and writes the marker; Setpgid makes it a group leader, so
	// kill(-pid) hits it exactly the way Stop()/interruptPID hit pi.
	cmd := exec.Command("/bin/sh", "-c",
		"trap 'echo hit > "+marker+"; exit 0' INT; while :; do sleep 0.05; done")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start interrupt probe: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL) // whole group, like Stop()
		_, _ = cmd.Process.Wait()
	})
	// Let the trap install before any signal is sent; the marker must NOT be
	// present yet.
	time.Sleep(150 * time.Millisecond)
	return pid
}

func markerPresent(t *testing.T, marker string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func ctrlText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(job.Ctrl(name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// armInterruptJob writes a job named name and returns a supervisor with its
// runner marked live at the given pid. Steer never reads the brief file, so
// the brief path may be dangling.
func armInterruptJob(t *testing.T, name string, pid int, steerWait time.Duration) *Supervisor {
	t.Helper()
	writeJob(t, job.Job{
		Name: name, Brief: filepath.Join(t.TempDir(), "brief.md"), Worktree: t.TempDir(),
		SessionName: name, MaxRounds: 1, TimeoutS: 20, PiBin: "true",
	})
	s := newTestSupervisor(t)
	s.steerWait = steerWait
	armFakeRound(t, s, name, pid)
	return s
}

// Steer --interrupt on a live round: the SIGINT reaches pi's process group
// (the marker appears) AND the frame is still delivered. Signaling must not
// replace delivery — that is the whole contract of ADR-0007.
func TestSteerInterruptSignalsGroupAndDelivers(t *testing.T) {
	testEnv(t)
	marker := filepath.Join(t.TempDir(), "interrupted")
	pid := interruptProbe(t, marker)

	s := armInterruptJob(t, "intr", pid, 6*time.Second)
	// Play the client: wait for the frame to land in the ctrl file, then
	// append the forwarded ack. This confirms delivery, proving the interrupt
	// did not block or prevent the frame from reaching pi.
	go func() {
		deadline := time.After(5 * time.Second)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-deadline:
				return
			case <-tick.C:
			}
			id, ok := lastFrameID(t, job.Ctrl("intr"))
			if !ok || id == "" {
				continue
			}
			appendAck(t, job.Ack("intr"), job.AckRecord{
				ID: id, Outcome: job.AckForwarded, Type: "prompt",
			})
			return
		}
	}()

	rep, err := s.Steer("intr", "STOP and switch to the failing test", false, true)
	if err != nil {
		t.Fatalf("interrupt steer: %v (rep %+v)", err, rep)
	}
	if !rep.Interrupted {
		t.Fatal("report must mark the interrupt as sent")
	}
	if !markerPresent(t, marker, 3*time.Second) {
		t.Fatal("SIGINT never reached the pi process group (marker absent)")
	}
	if !strings.Contains(ctrlText(t, "intr"), "STOP and switch") {
		t.Fatal("interrupt steer did not write the prompt frame")
	}
}

// The frame is NOT written when the interrupt cannot be sent: the operator
// asked for the running turn to be dropped, so a silent fallback to a plain
// queued steer would read as an interrupt that never happened (ADR-0005).
func TestSteerInterruptFailureWritesNoFrame(t *testing.T) {
	testEnv(t)
	// A live round whose pid is a real-but-dead group: the runner looks live
	// (active && pid>0) but kill(-pid) fails with ESRCH, which is the case
	// where a silent fallback to a plain queued steer would be worst.
	dead := exec.Command("/bin/sh", "-c", "exit 0")
	dead.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := dead.Start(); err != nil {
		t.Fatal(err)
	}
	deadPID := dead.Process.Pid
	_, _ = dead.Process.Wait() // reaped: the pid is now stale

	s := armInterruptJob(t, "intr-bad", deadPID, 2*time.Second)

	rep, err := s.Steer("intr-bad", "text", false, true)
	if err == nil {
		t.Fatal("a steer whose interrupt cannot be sent must fail")
	}
	if rep.Interrupted {
		t.Fatal("report must not claim an interrupt that was not sent")
	}
	if !strings.Contains(rep.Detail, "interrupt") {
		t.Fatalf("detail should explain the failed interrupt, got %q", rep.Detail)
	}
	if got := ctrlText(t, "intr-bad"); strings.TrimSpace(got) != "" {
		t.Fatalf("no frame may be written when the interrupt fails, ctrl has %q", got)
	}
}

// A plain steer (no --interrupt) must NOT signal pi: the flag is opt-in.
func TestSteerWithoutInterruptDoesNotSignal(t *testing.T) {
	testEnv(t)
	marker := filepath.Join(t.TempDir(), "interrupted")
	pid := interruptProbe(t, marker)

	s := armInterruptJob(t, "plain", pid, 2*time.Second)

	rep, _ := s.Steer("plain", "just a note", false, false)
	if rep.Interrupted {
		t.Fatal("a plain steer must not report an interrupt")
	}
	// Give any (incorrect) signal time to show up, then assert none did.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a plain steer must not SIGINT pi")
	}
}
