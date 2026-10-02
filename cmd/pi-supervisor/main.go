// pi-supervisor — systemd user daemon supervising long-running pi RPC
// delegations (spawn/resume rounds, session monitoring, instant-exit
// strikes) with a Unix-socket control API.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"pi-supervisor/internal/control"
	"pi-supervisor/internal/job"
	"pi-supervisor/internal/notify"
	"pi-supervisor/internal/supervisor"
)

func socketPath() string {
	if p := os.Getenv("PI_SUPERVISOR_SOCK"); p != "" {
		return p
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = "/tmp"
	}
	return filepath.Join(rt, "pi-supervisor.sock")
}

// runDaemon is ExecStart: load jobs, serve control socket, monitor, ready.
func runDaemon() {
	_ = os.MkdirAll(job.JobsDir(), 0o755)
	_ = os.MkdirAll(job.StateDir(), 0o755)

	sup := supervisor.New()
	if err := sup.LoadJobs(); err != nil {
		fmt.Fprintf(os.Stderr, "load jobs: %v\n", err)
	}
	stop := make(chan struct{})
	go control.Serve(socketPath(), sup, stop)
	go sup.Monitor(stop)
	// One sd_notify beat carries both the watchdog ping and STATUS=<n>
	// parallel pi session(s) running, visible in `systemctl status`.
	go notify.Beat(stop, sup.RunningCount)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		_ = notify.Send("STOPPING=1")
		sup.Shutdown()
		os.Exit(0)
	}()

	if err := notify.Send("READY=1"); err != nil {
		fmt.Fprintf(os.Stderr, "sd_notify READY: %v\n", err)
	}
	fmt.Println("pi-supervisor ready at", socketPath())
	select {} // loops run in goroutines
}

// ctl talks to the running daemon over the control socket.
func ctl(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pi-supervisor status [job] | start|stop <job> | steer <job> <text...> | logs <job> [n] | reload")
		os.Exit(2)
	}
	var req control.Request
	switch args[0] {
	case "status":
		req.Cmd = "status"
		if len(args) > 1 {
			req.Job = args[1]
		}
	case "start", "stop":
		if len(args) < 2 {
			fatalf("%s requires a job name", args[0])
		}
		req.Cmd, req.Job = args[0], args[1]
	case "steer":
		if len(args) < 3 {
			fatalf("steer requires <job> <text...>")
		}
		req.Cmd, req.Job, req.Text = "steer", args[1], strings.Join(args[2:], " ")
	case "logs":
		if len(args) < 2 {
			fatalf("logs requires a job name")
		}
		req.Cmd, req.Job = "logs", args[1]
		if len(args) > 2 {
			req.N, _ = strconv.Atoi(args[2])
		}
	case "reload":
		req.Cmd = "reload"
	default:
		fatalf("unknown ctl command %q", args[0])
	}

	c, err := net.Dial("unix", socketPath())
	if err != nil {
		fatalf("daemon not reachable at %s: %v", socketPath(), err)
	}
	defer c.Close()
	line, _ := json.Marshal(req)
	if _, err := c.Write(append(line, '\n')); err != nil {
		fatalf("write: %v", err)
	}
	respData, err := io.ReadAll(c)
	if err != nil {
		fatalf("read: %v", err)
	}
	var resp control.Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		fatalf("bad response: %s", string(respData))
	}
	if !resp.OK {
		fmt.Fprintln(os.Stderr, "error:", resp.Error)
		os.Exit(1)
	}
	switch data := resp.Data.(type) {
	case []string:
		for _, l := range data {
			fmt.Println(l)
		}
	default:
		out, _ := json.MarshalIndent(resp.Data, "", "  ")
		fmt.Println(string(out))
	}
}

func fatalf(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: pi-supervisor run | status [job] | start|stop <job> | steer <job> <text...> | logs <job> [n] | reload")
	}
	switch os.Args[1] {
	case "run":
		runDaemon()
	case "status", "start", "stop", "steer", "logs", "reload":
		ctl(os.Args[1:])
	default:
		fatalf("unknown command %q", os.Args[1])
	}
	_ = bufio.NewReader // silence if unused after refactors
}
