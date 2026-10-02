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
// status line. countFn supplies the number of running sessions for STATUS=;
// when it returns an error the ping still goes out alone.
func Beat(stop <-chan struct{}, countFn func() int) {
	usec := os.Getenv("WATCHDOG_USEC")
	interval := 30 * time.Second
	if usec != "" {
		if us, err := strconv.Atoi(usec); err == nil && us > 0 {
			interval = time.Duration(us/2) * time.Microsecond
		}
	}
	// Always report status, even when systemd didn't arm a watchdog, so
	// `systemctl status` stays live in the non-notify case too.
	if os.Getenv("WATCHDOG_USEC") == "" {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	lastCount := -1
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if countFn == nil {
				_ = Send("WATCHDOG=1")
				continue
			}
			n := countFn()
			// Skip the write when nothing changed: sd_notify is a syscall
			// per beat and the count is usually static between round ends.
			if n == lastCount {
				_ = Send("WATCHDOG=1")
				continue
			}
			lastCount = n
			_ = Send(fmt.Sprintf("STATUS=%d parallel pi session(s) running\nWATCHDOG=1", n))
		}
	}
}

// Watchdog answers systemd's watchdog (WATCHDOG_USEC) with WATCHDOG=1 pings
// at half the interval, until stop closes. No-op when unset.
func Watchdog(stop <-chan struct{}) {
	Beat(stop, nil)
}
