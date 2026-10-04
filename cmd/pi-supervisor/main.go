// pi-supervisor — systemd user daemon supervising long-running pi RPC
// delegations (spawn/resume rounds, session monitoring, instant-exit
// strikes) with a Unix-socket control API.
//
// The CLI is cobra (command tree, flags, help, generated completions) with
// viper for configuration precedence: explicit flag > PI_SUPERVISOR_* env >
// optional config file > built-in default. Exit codes are a contract the test
// suite and scripts depend on: 2 = usage/validation, 1 = runtime/daemon
// refused, 0 = success. See the exit constants and main().
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"pi-supervisor/internal/control"
	"pi-supervisor/internal/job"
	"pi-supervisor/internal/notify"
	"pi-supervisor/internal/supervisor"
	"pi-supervisor/internal/version"
)

// Exit codes, kept as named constants because the test suite asserts on them.
const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

// Sentinels that classify a failure as usage-class (exit 2), matching the
// pre-cobra behavior: errUsage covers bad operator input, errUnreachable
// covers "no daemon on the socket" (there is simply nothing to answer).
var (
	errUsage       = errors.New("usage")
	errUnreachable = errors.New("daemon not reachable")
)

func socketPath() string {
	if p := os.Getenv("PI_SUPERVISOR_SOCK"); p != "" {
		return p
	}
	if p := viper.GetString("socket"); p != "" {
		return p
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = "/tmp"
	}
	return filepath.Join(rt, "pi-supervisor.sock")
}

// initConfig wires viper: optional ~/.config/pi-supervisor/config.yaml plus
// PI_SUPERVISOR_SOCKET / PI_SUPERVISOR_CONFIG env overrides for `socket`. A
// missing config file is not an error — every value has an env or default
// fallback.
func initConfig() {
	viper.SetDefault("socket", "")
	viper.SetDefault("version", "dev") // overridden by -ldflags -X at release build
	if v := os.Getenv("PI_SUPERVISOR_SOCKET"); v != "" {
		viper.SetDefault("socket", v)
	}
	if cfg := os.Getenv("PI_SUPERVISOR_CONFIG"); cfg != "" {
		viper.SetConfigFile(cfg)
	} else {
		home, _ := os.UserHomeDir()
		viper.AddConfigPath(filepath.Join(home, ".config", "pi-supervisor"))
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
	}
	viper.AutomaticEnv()
	_ = viper.ReadInConfig() // absent config is fine
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
	// Sweep unacked bulk_resolve requests: an unattended campaign must never
	// wedge on a gate nobody will visit (ADR-0012 §2.3).
	go sup.ExpireAcks(stop)
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

// sendCTL performs one-shot request/response against the running daemon and
// renders the reply. The returned error is mapped to an exit code by main().
func sendCTL(req control.Request) error {
	c, err := net.Dial("unix", socketPath())
	if err != nil {
		return fmt.Errorf("%w at %s: %w", errUnreachable, socketPath(), err)
	}
	defer c.Close()
	line, _ := json.Marshal(req)
	if _, err := c.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	respData, err := io.ReadAll(c)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var resp control.Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		return fmt.Errorf("bad response: %s", string(respData))
	}
	if !resp.OK {
		if req.Cmd == "steer" {
			// The report is the answer even when the steer failed.
			printSteer(resp)
		}
		return errors.New(resp.Error)
	}
	if req.Cmd == "steer" {
		printSteer(resp)
		return nil
	}
	// A JSON array decodes into []any, not []string: print a string array
	// (logs) line by line, anything else as indented JSON.
	if arr, isArr := resp.Data.([]any); isArr && len(arr) > 0 {
		strs := make([]string, 0, len(arr))
		for _, v := range arr {
			s, ok := v.(string)
			if !ok {
				strs = nil
				break
			}
			strs = append(strs, s)
		}
		if strs != nil {
			for _, s := range strs {
				// sendCTL's own stdout: there is no upstream to report a write
				// failure to, and the exit code stays 0 either way.
				_, _ = fmt.Println(s)
			}
			return nil
		}
	}
	out, _ := json.MarshalIndent(resp.Data, "", "  ")
	fmt.Println(string(out))
	if line := prStatusLine(resp.Data); line != "" {
		fmt.Println(line)
	}
	return nil
}

// prStatusLine renders the trailing `pr <url>` field of a single-job status,
// or "" when no PR was linked (ADR-0006). The JSON already carries pr_url;
// this is the human/grep-friendly one-liner. A list status decodes as []any
// and yields "".
func prStatusLine(data any) string {
	m, ok := data.(map[string]any)
	if !ok {
		return ""
	}
	url, _ := m["pr_url"].(string)
	if url == "" {
		return ""
	}
	return "pr " + url
}

// printSteer renders a steer report: where the frame was sent, then what pi
// actually did with it (ADR-0005). The outcome line is the whole point —
// never print a bare success for a frame nobody acknowledged. An interrupt
// steer also states that the running turn was asked to stop (ADR-0007).
func printSteer(resp control.Response) {
	raw, err := json.Marshal(resp.Data)
	if err != nil {
		fmt.Printf("steer: %v (no report)\n", err)
		return
	}
	var r job.SteerReport
	if err := json.Unmarshal(raw, &r); err != nil {
		fmt.Printf("steer: %s (no report)\n", string(raw))
		return
	}
	live := "no live round"
	if r.LiveRound {
		live = "live round"
	}
	outcome := r.Outcome
	if outcome == "" {
		outcome = "not sent"
	}
	fmt.Printf("steer  job=%s round=%d (%s, pi pid %d)\n", r.Job, r.Round, live, r.ClientPID)
	if r.Interrupted {
		fmt.Printf("       interrupt  SIGINT sent to pi process group (-%d): current turn asked to stop\n", r.ClientPID)
	}
	fmt.Printf("       session  %s\n", orDash(r.SessionPath))
	fmt.Printf("       ctrl     %s\n", orDash(r.CtrlPath))
	if r.FrameID != "" {
		fmt.Printf("       frame    %s\n", r.FrameID)
	}
	fmt.Printf("       ack      %s\n", orDash(r.AckPath))
	line := "       outcome " + outcome
	if r.DelayMS > 0 {
		line += fmt.Sprintf(" after %dms", r.DelayMS)
	}
	if r.Outcome != "" {
		line += fmt.Sprintf("  (waited %dms)", r.WaitedMS)
	}
	fmt.Println(line)
	if r.Detail != "" {
		fmt.Printf("       detail  %s\n", r.Detail)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// watchFooter prints what an LLM should do next with this event, targeted at
// the job's worktree (ADR-0003: the message instructs the next action).
func watchFooter(ev map[string]any) {
	name, _ := ev["job"].(string)
	wt, _ := ev["worktree"].(string)
	if wt == "" {
		wt = "."
	}
	kind, _ := ev["event"].(string)
	info, _ := ev["info"].(string)
	rc, _ := ev["rc"].(float64)
	round, _ := ev["round"].(float64)
	sess, _ := ev["session_path"].(string)

	// Transcript tail: the last couple of session JSONL lines, plus where the
	// rest lives. Missing/empty transcript prints nothing extra.
	if sess != "" {
		if lines := job.TailLines(sess, 2, 300); len(lines) > 0 {
			fmt.Printf("last session JSONL lines (%s):\n", sess)
			for _, ln := range lines {
				fmt.Printf("  %s\n", ln)
			}
			fmt.Printf("  (rest of the transcript: `tail -n 50 %s` or pi_session.py watch)\n", sess)
		}
	}

	fmt.Println("—")
	if pr, _ := ev["pr_url"].(string); pr != "" {
		fmt.Printf("pull request: %s\n", pr)
	}
	switch kind {
	case "round_done":
		rcmsg := "rc=0"
		if rc != 0 {
			rcmsg = fmt.Sprintf("rc=%d", int(rc))
		}
		fmt.Printf("LLM next steps for %s (round %v, %s):\n", name, int(round), rcmsg)
		fmt.Printf("  1. confirm only expected changes — `git -C %s diff --stat`\n", wt)
		fmt.Printf("   2. scan the round log — `tail -n 40 /tmp/pi_%s_run.log`\n", name)
		if rc != 0 {
			fmt.Printf("   3. off-course or error — `pi-supervisor steer %s \"...\"` (end of turn) or\n", name)
			fmt.Printf("      `pi-supervisor steer --interrupt %s \"...\"` to stop mid-turn. Then proceed\n", name)
		}
		fmt.Printf("  re-arm: pi-supervisor watch %s   (background+notify=true)\n", name)
	case "instant_exit":
		fmt.Printf("LLM: context-exhaustion strike (%s). Check provider errors in\n", info)
		fmt.Printf("`/tmp/pi_%s_run.log`; `pi-supervisor steer %s \"...\"` or `--set-model`,\n", name, name)
		fmt.Printf("or 3 strikes -> fatal. Re-arm: pi-supervisor watch %s\n", name)
	case "backoff":
		fmt.Printf("Idle — %s before the next round, no action needed.\n", info)
		fmt.Printf("Re-arm: pi-supervisor watch %s   (background+notify=true)\n", name)
	case "done":
		fmt.Printf("THE RUN IS OVER — %s reached its marker.\n", name)
		fmt.Printf("LLM: confirm deliverables — `git -C %s diff --stat` and the final report.\n", wt)
		fmt.Println("No re-arm: the job will not emit further events.")
	case "fatal":
		fmt.Printf("RUN HALTED — %s is fatal: %s\n", name, info)
		fmt.Printf("LLM: review `/tmp/pi_%s_run.log` + final report; resume later with\n", name)
		fmt.Printf("`pi-supervisor start %s` after correcting, or kill if settled. No re-arm.\n", name)
	case "stopped":
		fmt.Printf("operator stop — %s paused at round %.0f.\n", name, round)
		fmt.Printf("Re-arm: pi-supervisor start %s ; pi-supervisor watch %s\n", name, name)
	case "job_started":
		fmt.Printf("Job %s launched at %s.\n", name, wt)
		fmt.Printf("Monitor: pi-supervisor watch %s   | status: pi-supervisor status %s\n", name, name)
	default:
		fmt.Printf("next: re-arm (background+notify): pi-supervisor watch %s | status: pi-supervisor status %s\n", name, name)
	}
}

// watchCtl blocks on the control socket and prints events as they arrive.
// Exit 0 on an event (with a footer of next steps), 1 when the connection is
// lost (daemon restart — re-arm), 2 on usage errors. It owns its own exits
// because it streams and must distinguish each terminal condition; the tests
// pin all three codes.
func watchCtl(req control.Request, terminal bool) {
	c, err := net.Dial("unix", socketPath())
	if err != nil {
		fatalf("daemon not reachable at %s: %v", socketPath(), err)
	}
	defer c.Close()
	line, _ := json.Marshal(req)
	if _, err := c.Write(append(line, '\n')); err != nil {
		fatalf("write: %v", err)
	}

	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var resp control.Response
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			continue
		}
		if !resp.OK {
			fmt.Fprintln(os.Stderr, "error:", resp.Error)
			os.Exit(2)
		}
		ev, isEvent := resp.Data.(map[string]any)
		if isEvent && ev["type"] == "watch_ack" {
			// First message is the ack, not an event — keep waiting.
			w := req.Job
			if w == "" {
				w = "*"
			}
			fmt.Println("watching", w, "— blocked until the supervisor sends data (Ctrl-C to stop)")
			continue
		}
		if !isEvent {
			continue // unexpected non-map payload; ignore
		}
		out, _ := json.Marshal(ev)
		fmt.Println(string(out))
		kind, _ := ev["event"].(string)
		watchFooter(ev)
		switch kind {
		case "done", "fatal", "stopped":
			os.Exit(0)
		default:
			if !terminal {
				os.Exit(0)
			}
		}
	}
	// Connection closed by the daemon or lost mid-stream.
	fmt.Fprintln(os.Stderr, "watch connection lost (daemon restart?) — check: systemctl --user status pi-supervisor, then re-arm: pi-supervisor watch", req.Job)
	os.Exit(1)
}

func fatalf(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(exitUsage)
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "pi-supervisor",
		Short: "Supervise long-running pi RPC delegations",
		Long: "pi-supervisor runs as a systemd --user daemon that spawns/resumes\n" +
			"pi RPC rounds, monitors sessions, and answers queries over a\n" +
			"Unix socket. This CLI is the control surface: run the daemon\n" +
			"(`run`) or query/steer it (status/start/stop/restart/steer/logs/watch).\n" +
			"Use `restart --fresh` to discard a poisoned session and start anew.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// No subcommand = usage-class failure, as before the cobra migration.
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("%w: see 'pi-supervisor --help'", errUsage)
		},
	}
	// Flag parse errors are usage errors (exit 2), matching pre-cobra.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fmt.Errorf("%w: %w", errUsage, err)
	})
	root.CompletionOptions.HiddenDefaultCmd = true // we ship our own `completion install`

	root.AddCommand(
		newRunCmd(),
		newStatusCmd(),
		newJobCmd("start"),
		newJobCmd("stop"),
		newRestartCmd(),
		newSteerCmd(),
		newLogsCmd(),
		newReloadCmd(),
		newWatchCmd(),
		newReviewCmd(),
		newAckCmd(),
		newReviewActionCmd(),
		newVersionCmd(),
		newCompletionCmd(root),
	)
	return root
}

func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "run",
		Short:  "Run the supervisor daemon (ExecStart for the systemd unit)",
		Hidden: true, // invoked by systemd, not by hand
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			runDaemon()
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "status [job]",
		Short:             "Show job status (all jobs when no name is given)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			req := control.Request{Cmd: "status"}
			if len(args) == 1 {
				req.Job = args[0]
			}
			return sendCTL(req)
		},
	}
}

// newRestartCmd implements `pi-supervisor restart <job> [--fresh]` (ADR-0010).
// Without --fresh it's a plain stop+start (resume the same session). With
// --fresh the daemon quarantines the transcript and starts a brand-new one.
func newRestartCmd() *cobra.Command {
	var fresh bool
	c := &cobra.Command{
		Use:               "restart <job>",
		Short:             "Restart a job (stop then start, or start fresh with --fresh)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sendCTL(control.Request{Cmd: "restart", Job: args[0], Fresh: fresh})
		},
	}
	c.Flags().BoolVar(&fresh, "fresh", false,
		"quarantine the current session transcript and start a brand-new one "+
			"(round counter resets, brief+cont re-read from disk)")
	return c
}

func newJobCmd(verb string) *cobra.Command {
	return &cobra.Command{
		Use:               verb + " <job>",
		Short:             strings.ToUpper(verb[:1]) + verb[1:] + " a job",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sendCTL(control.Request{Cmd: verb, Job: args[0]})
		},
	}
}

func newSteerCmd() *cobra.Command {
	var noWait, interrupt bool
	c := &cobra.Command{
		Use:   "steer <job> <text...>",
		Short: "Send a steer message to a running round",
		Long: "steer wraps prose into a prompt frame and delivers it to the live\n" +
			"round's pi process (ADR-0005). It reports where the frame went and\n" +
			"what pi did with it. With --interrupt, pi's process group is\n" +
			"SIGINTed first so the current turn is asked to stop and this steer\n" +
			"is what pi picks up next (ADR-0007).",
		Args: cobra.MinimumNArgs(2), // <job> + at least one text token
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return completeJobNames(cmd, args, toComplete)
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return sendCTL(control.Request{
				Cmd:       "steer",
				Job:       args[0],
				Text:      strings.Join(args[1:], " "),
				NoWait:    noWait,
				Interrupt: interrupt,
			})
		},
	}
	c.Flags().BoolVarP(&noWait, "no-wait", "n", false, "return once the frame is on disk, without waiting for pi's ack")
	c.Flags().BoolVarP(&interrupt, "interrupt", "i", false, "SIGINT pi's process group first so the current turn stops and this steer is next")
	return c
}

func newLogsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs <job> [n]",
		Short: "Show the last n lines of a job's run log (default all)",
		Args:  cobra.RangeArgs(1, 2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return completeJobNames(cmd, args, toComplete)
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			req := control.Request{Cmd: "logs", Job: args[0]}
			if len(args) > 1 {
				req.N, _ = strconv.Atoi(args[1])
			}
			return sendCTL(req)
		},
	}
}

func newReloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Reload job definitions from disk",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sendCTL(control.Request{Cmd: "reload"})
		},
	}
}

func newWatchCmd() *cobra.Command {
	var terminal bool
	c := &cobra.Command{
		Use:               "watch [job]",
		Short:             "Stream job events until a terminal one (default: all jobs)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			req := control.Request{Cmd: "watch"}
			if len(args) == 1 {
				req.Job = args[0]
			}
			watchCtl(req, terminal) // watchCtl owns its exit codes
			return nil
		},
	}
	c.Flags().BoolVarP(&terminal, "terminal", "t", false, "keep streaming after non-terminal events")
	return c
}

// newVersionCmd prints the build-time version (ADR-0009). The version is
// injected at link time by -ldflags="-X internal/version.ver=<commit>";
// without injection it reads "dev", so a plain `go build` is still identified.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the pi-supervisor version",
		Long: "pi-supervisor is versioned by the git commit it was built from\n" +
			"(" + viper.GetString("version") + "). When built from a clean tree,\n" +
			"this prints that commit id; a developer `go build` without -ldflags\n" +
			"prints 'dev'.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Println("pi-supervisor", version.Version())
			return nil
		},
	}
}

// completeJobNames offers known job names (from ~/.pi/supervisor/jobs/*.json)
// for shell completion. It never touches the daemon and never fails the tab.
func completeJobNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	entries, err := os.ReadDir(job.JobsDir())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

func main() {
	initConfig()
	root := newRootCmd()
	_, err := root.ExecuteC()
	if err == nil {
		os.Exit(exitOK)
	}
	// One error print for every failure path. Exit-code mapping:
	//   usage / no-daemon        -> 2 (pre-cobra behavior, pinned by tests)
	//   daemon refused / bad rsp -> 1
	//   cobra arg/flag validation -> 2
	fmt.Fprintln(os.Stderr, "error:", err)
	switch {
	case errors.Is(err, errUsage), errors.Is(err, errUnreachable), isCobraUsageError(err):
		os.Exit(exitUsage)
	default:
		// A review refusal carries its own exit class: 2 when the caller must
		// change the call (usage / no live round / not-answered), 1 when it may
		// retry (auth / rate limit / GitHub error). Branching on the reason
		// keeps that decision out of the message text (ADR-0012 §2.4).
		var rf *reviewFailure
		if errors.As(err, &rf) {
			os.Exit(rf.ExitCode())
		}
		os.Exit(exitRuntime)
	}
	_ = bufio.NewReader // silence if unused after refactors
}

// isCobraUsageError reports whether err came from cobra's own command lookup /
// argument validation (as opposed to a RunE handler's own error). Those are
// usage-class (exit 2). Cobra exposes no typed error for these, so match the
// stable message prefixes it produces.
func isCobraUsageError(err error) bool {
	msg := err.Error()
	for _, s := range []string{
		"unknown command",
		"unknown shorthand flag",
		"unknown flag",
		"flag needs an argument",
		"required flag",
		"invalid argument",
		// cobra's Args validators: ExactArgs -> "accepts 1 arg(s)", NoArgs /
		// MaximumNArgs -> "accepts no/at most ...", RangeArgs -> "accepts
		// between ...", MinimumNArgs -> "requires at least ..."
		"accepts ",
		"requires at least",
		"requires exactly",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
