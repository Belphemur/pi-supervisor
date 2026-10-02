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

// Watchdog answers systemd's watchdog (WATCHDOG_USEC) with WATCHDOG=1 pings
// at half the interval, until stop closes. No-op when unset.
func Watchdog(stop <-chan struct{}) {
	usec := os.Getenv("WATCHDOG_USEC")
	if usec == "" {
		return
	}
	us, err := strconv.Atoi(usec)
	if err != nil || us <= 0 {
		return
	}
	t := time.NewTicker(time.Duration(us/2) * time.Microsecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_ = Send("WATCHDOG=1")
		}
	}
}
