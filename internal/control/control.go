// Package control serves the Unix-socket request/response API and defines
// the wire protocol shared with the ctl subcommand.
package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"pi-supervisor/internal/events"
)

// Handler is the server-side interface the socket serves; the supervisor
// package implements it (consumer-defined interface, no import cycle).
type Handler interface {
	Status(job string) (any, error)
	StatusAll() any
	Start(name string) error
	Stop(name string) error
	Steer(name, text string) error
	Logs(name string, n int) ([]string, error)
	Reload() error
}

// Watcher is the optional push extension: cmd "watch" streams events to the
// client instead of answering once. Implemented by Supervisor.
type Watcher interface {
	// Watch subscribes to a job's events ("" = all jobs). A non-nil third
	// return means the target is already terminal: send it and close.
	Watch(job string) (<-chan events.Event, func(), *events.Event)
}

// Request is one control command (one JSON line per connection).
type Request struct {
	Cmd  string `json:"cmd"`            // status|start|stop|steer|logs|reload
	Job  string `json:"job,omitempty"`  // target job ("" = all, for status)
	Text string `json:"text,omitempty"` // steer payload
	N    int    `json:"n,omitempty"`    // logs line count
}

// Response is the single reply.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Data  any    `json:"data,omitempty"`
}

// Serve accepts JSON-line requests until stop closes.
func Serve(sockPath string, h Handler, stop chan struct{}) {
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control socket: %v\n", err)
		os.Exit(1)
	}
	_ = os.Chmod(sockPath, 0o600)
	go func() {
		<-stop
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-stop:
				return
			default:
				// A transient accept failure (EMFILE, ECONNABORTED) must not
				// turn the accept loop into a hot spin: back off briefly and
				// keep serving.
				time.Sleep(50 * time.Millisecond)
				continue
			}
		}
		go func(c net.Conn) {
			defer c.Close()
			line, err := bufio.NewReader(c).ReadBytes('\n')
			if err != nil && len(line) == 0 {
				return
			}
			var req Request
			if json.Unmarshal(line, &req) == nil && req.Cmd == "watch" {
				if w, ok := h.(Watcher); ok {
					serveWatch(c, h, w, req)
					return
				}
				writeOne(c, Response{OK: false, Error: "watch unsupported"})
				return
			}
			writeOne(c, dispatch(h, line))
		}(conn)
	}
}

// writeOne writes one Response line; false = the client is gone.
func writeOne(c net.Conn, resp Response) bool {
	data, err := json.Marshal(resp)
	if err != nil {
		return false
	}
	_, werr := c.Write(append(data, '\n'))
	return werr == nil
}

// serveWatch holds the connection open and pushes events until a terminal
// one (or the client disconnects). Filtering by job happens here so the
// broker stays unfiltered and cheap.
func serveWatch(c net.Conn, h Handler, w Watcher, req Request) {
	label := req.Job
	if label == "" {
		label = "*"
	}
	// Unknown-job guard: error out and close instead of blocking forever.
	if req.Job != "" {
		if _, err := h.Status(req.Job); err != nil {
			writeOne(c, Response{OK: false, Error: err.Error()})
			return
		}
	}
	ch, cancel, pre := w.Watch(req.Job)
	if pre != nil {
		// Requirement 4: a finished run answers immediately, no blocking.
		_ = writeOne(c, Response{OK: true, Data: *pre})
		return
	}
	defer cancel()
	if !writeOne(c, Response{OK: true, Data: map[string]any{"type": "watch_ack", "watching": label}}) {
		return
	}
	for {
		e, ok := <-ch
		if !ok {
			return
		}
		if req.Job != "" && e.Job != req.Job {
			continue
		}
		if !writeOne(c, Response{OK: true, Data: e}) {
			return
		}
		if e.Terminal() {
			return
		}
	}
}

func dispatch(h Handler, raw []byte) Response {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return Response{OK: false, Error: "bad json: " + err.Error()}
	}
	switch req.Cmd {
	case "status":
		if req.Job != "" {
			data, err := h.Status(req.Job)
			if err != nil {
				return Response{OK: false, Error: err.Error()}
			}
			return Response{OK: true, Data: data}
		}
		return Response{OK: true, Data: h.StatusAll()}
	case "start":
		if err := h.Start(req.Job); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "stop":
		if err := h.Stop(req.Job); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "steer":
		if err := h.Steer(req.Job, req.Text); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "logs":
		data, err := h.Logs(req.Job, req.N)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Data: data}
	case "reload":
		if err := h.Reload(); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	default:
		return Response{OK: false, Error: "unknown cmd " + req.Cmd}
	}
}
