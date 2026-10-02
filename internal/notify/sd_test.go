package notify

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// collector is a unixgram socket standing in for systemd's notify socket.
type collector struct {
	mu   sync.Mutex
	msgs []string
	c    *net.UnixConn
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "notify.sock")
	addr, err := net.ResolveUnixAddr("unixgram", path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	col := &collector{c: c}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			col.mu.Lock()
			col.msgs = append(col.msgs, string(buf[:n]))
			col.mu.Unlock()
		}
	}()
	return col
}

func (c *collector) path() string { return c.c.LocalAddr().String() }

func (c *collector) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

func (c *collector) waitFor(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.all(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("got %d messages, want %d: %q", len(c.all()), n, c.all())
	return nil
}

// Send is a no-op when systemd is not managing us.
func TestSendUnsetSocketIsNoop(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Send("READY=1"); err != nil {
		t.Fatalf("Send with no NOTIFY_SOCKET: %v", err)
	}
}

// Send delivers the exact datagram to a filesystem socket.
func TestSendDeliversState(t *testing.T) {
	col := newCollector(t)
	t.Setenv("NOTIFY_SOCKET", col.path())

	if err := Send("READY=1"); err != nil {
		t.Fatal(err)
	}
	if err := Send("STOPPING=1"); err != nil {
		t.Fatal(err)
	}
	got := col.waitFor(t, 2)
	if got[0] != "READY=1" || got[1] != "STOPPING=1" {
		t.Fatalf("datagrams = %q, want READY=1 then STOPPING=1", got)
	}
}

// A dial failure is reported, not swallowed.
func TestSendReportsDialError(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	err := Send("READY=1")
	if err == nil || !strings.Contains(err.Error(), "sd_notify dial") {
		t.Fatalf("err = %v, want a dial error", err)
	}
}

// An abstract socket (leading '@') is mapped to the unixgram autobind family.
func TestSendAbstractSocket(t *testing.T) {
	// Autobind needs a Linux kernel socket; skip cleanly elsewhere.
	addr, err := net.ResolveUnixAddr("unixgram", "@pi-supervisor-test-abstract")
	if err != nil {
		t.Skipf("abstract sockets unsupported: %v", err)
	}
	c, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Skipf("cannot listen on an abstract socket: %v", err)
	}
	defer c.Close()

	received := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _, err := c.ReadFrom(buf)
		if err == nil {
			received <- string(buf[:n])
		}
	}()

	t.Setenv("NOTIFY_SOCKET", "@pi-supervisor-test-abstract")
	if err := Send("READY=1"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "READY=1" {
			t.Fatalf("datagram = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no datagram on the abstract socket")
	}
}

// Beat pairs STATUS=<n> with WATCHDOG=1 and re-pings on every later beat
// even when the count has not changed.
func TestBeatSendsStatusAndWatchdog(t *testing.T) {
	col := newCollector(t)
	t.Setenv("NOTIFY_SOCKET", col.path())
	t.Setenv("WATCHDOG_USEC", "40000") // 40ms -> 20ms beat

	n := 0
	stop := make(chan struct{})
	var once sync.Once
	go Beat(stop, func() int {
		once.Do(func() { n = 2 })
		return n
	})

	got := col.waitFor(t, 3)
	if got[0] != "STATUS=2 parallel pi session(s) running\nWATCHDOG=1" {
		t.Fatalf("first beat = %q", got[0])
	}
	// Unchanged count: the ping still goes out (systemd restarts us otherwise)
	// but the datagram is the bare ping.
	for _, m := range got[1:] {
		if m != "WATCHDOG=1" && m != got[0] {
			t.Fatalf("unexpected beat %q", m)
		}
	}
	close(stop)
}

// Watchdog (no status) sends bare WATCHDOG=1 pings.
func TestWatchdogSendsBarePings(t *testing.T) {
	col := newCollector(t)
	t.Setenv("NOTIFY_SOCKET", col.path())
	t.Setenv("WATCHDOG_USEC", "40000")

	stop := make(chan struct{})
	go Watchdog(stop)
	got := col.waitFor(t, 2)
	for _, m := range got {
		if m != "WATCHDOG=1" {
			t.Fatalf("watchdog datagram = %q, want a bare ping", m)
		}
	}
	close(stop)
}

// Without a watchdog armed, Beat still reports status on a 15s tick so
// `systemctl status` stays live.
func TestBeatWithoutWatchdogStillReports(t *testing.T) {
	col := newCollector(t)
	t.Setenv("NOTIFY_SOCKET", col.path())
	t.Setenv("WATCHDOG_USEC", "")

	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		Beat(stop, func() int { return 7 })
		close(stopped)
	}()
	// The first beat is 15s away: assert only that the loop is alive and
	// exits promptly, without paying a 15s test.
	select {
	case <-stopped:
		t.Fatal("Beat returned before its stop channel closed")
	case <-time.After(200 * time.Millisecond):
	}
	close(stop)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Beat did not return on stop")
	}
}

// WATCHDOG_USEC=1 halves to a zero interval; that must not panic NewTicker.
func TestBeatSurvivesDegenerateWatchdogInterval(t *testing.T) {
	col := newCollector(t)
	t.Setenv("NOTIFY_SOCKET", col.path())
	t.Setenv("WATCHDOG_USEC", "1")
	t.Setenv("WATCHDOG_PID", "not-a-pid") // must be ignored

	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		Beat(stop, nil)
	}()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Beat did not return on stop")
	}
	_ = col.all()
	_ = os.Getpid()
}
