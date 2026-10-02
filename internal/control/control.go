// Package control serves the Unix-socket request/response API and defines
// the wire protocol shared with the ctl subcommand.
package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
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
// Request is one control command (one JSON line per connection).
type Request struct {
	Cmd  string `json:"cmd"`            // status|start|stop|steer|logs|reload
	Job  string `json:"job,omitempty"`  // target job ("" = all, for status)
	Text string `json:"text,omitempty"` // steer payload
	N    int    `json:"n,omitempty"`    // logs line count
}

// Response is the single reply.
type Response struct {
	OK    bool        `json:"ok"`
	Error string      `json:"error,omitempty"`
	Data  interface{} `json:"data,omitempty"`
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
				continue
			}
		}
		go func(c net.Conn) {
			defer c.Close()
			line, err := bufio.NewReader(c).ReadBytes('\n')
			if err != nil && len(line) == 0 {
				return
			}
			resp := dispatch(h, line)
			data, _ := json.Marshal(resp)
			_, _ = c.Write(append(data, '\n'))
		}(conn)
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
