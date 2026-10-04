// Package notify implements a pure-Go sd_notify client (no cgo): READY/STOPPING
// state transitions and systemd watchdog pings over $NOTIFY_SOCKET.
package notify

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Send reports a state change to systemd. Abstract sockets (leading '@')
// are mapped to the Linux autobind family. Silent no-op when not
// systemd-managed ($NOTIFY_SOCKET unset).
func Send(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	if strings.HasPrefix(addr, "@") {
		uaddr := &net.UnixAddr{Name: "\x00" + addr[1:], Net: "unixgram"}
		c, err := net.DialUnix("unixgram", nil, uaddr)
		if err != nil {
			return fmt.Errorf("sd_notify dial: %w", err)
		}
		defer c.Close()
		_, err = c.Write([]byte(state))
		return err
	}
	c, err := net.Dial("unixgram", addr)
	if err != nil {
		return fmt.Errorf("sd_notify dial: %w", err)
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}

// Beat sends one combined notify datagram: the watchdog ping plus a human
// status line. countFn supplies the number of running sessions for STATUS=.
func Beat(stop <-chan struct{}, countFn func() int) {
	if countFn == nil {
		BeatStatus(stop, nil)
		return
	}
	BeatStatus(stop, func() (int, string) { return countFn(), "" })
}

// BeatStatus is Beat with a caller-supplied status line. statusFn returns the
// running session count plus a note appended to STATUS= (e.g. a FATAL job
// name). Either part may be empty; a nil statusFn pings the watchdog alone.
// The datagram is written only when the count OR the note changed, because
// sd_notify costs a syscall per beat and both parts are usually static
// between round ends.
func BeatStatus(stop <-chan struct{}, statusFn func() (int, string)) {
	usec := os.Getenv("WATCHDOG_USEC")
	interval := 30 * time.Second
	if usec != "" {
		// us/2 can round down to zero (WATCHDOG_USEC=1), and a non-positive
		// NewTicker interval panics — clamp to the 30s default.
		if us, err := strconv.Atoi(usec); err == nil && us > 0 {
			if half := time.Duration(us/2) * time.Microsecond; half > 0 {
				interval = half
			}
		}
	}
	// Always report status, even when systemd didn't arm a watchdog, so
	// `systemctl status` stays live in the non-notify case too.
	if usec == "" {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	lastCount := -1
	lastNote := "\x00" // never equal to a real note: forces the first write
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if statusFn == nil {
				_ = Send("WATCHDOG=1")
				continue
			}
			n, note := statusFn()
			note = oneLine(note)
			// Skip the write when nothing changed: sd_notify is a syscall
			// per beat and the count is usually static between round ends.
			if n == lastCount && note == lastNote {
				_ = Send("WATCHDOG=1")
				continue
			}
			lastCount, lastNote = n, note
			status := fmt.Sprintf("STATUS=%d parallel pi session(s) running", n)
			if note != "" {
				status += "; " + note
			}
			_ = Send(status + "\nWATCHDOG=1")
		}
	}
}

// oneLine folds a status note onto a single line: sd_notify's STATUS= ends at
// the first newline, and a note with a newline in it would truncate the
// watchdog ping on the following line.
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == '\r' || r == '\n'
	}), " ")
}

// Watchdog answers systemd's watchdog (WATCHDOG_USEC) with WATCHDOG=1 pings
// at half the interval, until stop closes. No-op when unset.
func Watchdog(stop <-chan struct{}) {
	Beat(stop, nil)
}
