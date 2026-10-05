package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"pi-supervisor/internal/control"
	"pi-supervisor/internal/review"
)

// reviewCtl speaks the review protocol over the control socket and renders the
// reply. Every path is non-interactive and every failure is a non-zero exit
// plus a machine-readable reason: the consumer is an LLM, not a human
// (ADR-0012 §8, Q10).
//
// Exit codes follow ADR-0008 and the review reason map: 2 = the call must
// change (usage, no live round, round mismatch, not-answered, unknown thread),
// 1 = refused at runtime (auth, rate limit, GitHub error).
func reviewCtl(req map[string]any, jsonOut bool) error {
	resp, err := postReview(req)
	if err != nil {
		return err
	}
	if jsonOut {
		out, mErr := json.Marshal(resp)
		if mErr != nil {
			return mErr
		}
		fmt.Println(string(out))
	} else if resp.OK {
		printReviewOK(resp)
	}
	if !resp.OK {
		return &reviewFailure{Reason: resp.Reason, Message: resp.Error}
	}
	return nil
}

// reviewFailure carries the refusal reason so main() can map it to an exit
// code without parsing the message.
type reviewFailure struct {
	Reason  string
	Message string
}

func (e *reviewFailure) Error() string {
	if e.Message == "" {
		return e.Reason
	}
	return e.Message
}

// ExitCode maps the reason onto the CLI contract. An empty/unknown reason is a
// runtime refusal (1), never a usage error — guessing "usage" would tell an
// agent to change a call that was actually fine.
func (e *reviewFailure) ExitCode() int {
	if r := review.Reason(e.Reason); r != "" {
		return r.ExitCode()
	}
	return exitRuntime
}

// postReview sends one review request and decodes the response.
func postReview(body map[string]any) (control.Response, error) {
	c, err := dial()
	if err != nil {
		return control.Response{}, err
	}
	defer c.Close()
	line, mErr := json.Marshal(body)
	if mErr != nil {
		return control.Response{}, mErr
	}
	if _, wErr := c.Write(append(line, '\n')); wErr != nil {
		return control.Response{}, fmt.Errorf("write: %w", wErr)
	}
	raw, rErr := readAll(c)
	if rErr != nil {
		return control.Response{}, rErr
	}
	var resp control.Response
	if uErr := json.Unmarshal(raw, &resp); uErr != nil {
		return control.Response{}, fmt.Errorf("bad response: %s", string(raw))
	}
	return resp, nil
}

// printReviewOK renders a success as compact key=value lines an agent can read
// without parsing JSON.
func printReviewOK(resp control.Response) {
	m, isMap := resp.Data.(map[string]any)
	if !isMap {
		if s, isStr := resp.Data.(string); isStr {
			fmt.Println(s)
			return
		}
		out, _ := json.MarshalIndent(resp.Data, "", "  ")
		fmt.Println(string(out))
		return
	}
	for _, k := range sortedKeys(m) {
		fmt.Printf("%s=%v\n", k, m[k])
	}
}

// newReviewCmd implements `pi-supervisor review <job> --pr N` (ADR-0012 §5).
func newReviewCmd() *cobra.Command {
	var (
		pr      int
		rounds  int
		kind    string
		auto    bool
		recheck bool
		asJSON  bool
	)
	c := &cobra.Command{
		Use:   "review <job>",
		Short: "Arm a daemon-orchestrated code-review campaign on a PR (ADR-0012)",
		Long: "Arm a review campaign: the daemon owns the outer loop (poll GitHub,\n" +
			"decide, enforce the round budget) while pi executes each round's fixes.\n" +
			"Use --pr for a manual campaign, or --auto to arm the trigger that\n" +
			"fires when the completion gate closes on an open PR.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --recheck is a distinct mode, not a campaign: it compares the
			// PR's current open-thread count against the baseline recorded
			// when the campaign ended. It needs no --pr (the baseline carries
			// the PR) and must not arm anything.
			if recheck {
				if pr != 0 || auto {
					return fmt.Errorf("%w: --recheck takes no --pr/--auto; the baseline already names the PR", errUsage)
				}
				return reviewCtl(map[string]any{
					"cmd":      "review_recheck",
					"job":      args[0],
					"blocking": true,
				}, asJSON)
			}
			if pr == 0 && !auto {
				return fmt.Errorf("%w: pass --pr <N> or --auto", errUsage)
			}
			if pr != 0 && auto {
				return fmt.Errorf("%w: --pr and --auto are mutually exclusive", errUsage)
			}
			if kind != "acceptance" && kind != "rebuttal" {
				return fmt.Errorf("%w: --type must be acceptance or rebuttal, got %q", errUsage, kind)
			}
			return reviewCtl(map[string]any{
				"cmd":    "review",
				"job":    args[0],
				"pr":     pr,
				"rounds": rounds,
				"type":   kind,
				"auto":   auto,
			}, asJSON)
		},
	}
	c.Flags().IntVar(&pr, "pr", 0, "pull request number to review")
	c.Flags().IntVar(&rounds, "rounds", 5,
		"campaign round budget (review rounds are hours-long; 0 derives it from the open-thread count)")
	c.Flags().StringVar(&kind, "type", "acceptance",
		"round character: acceptance (treat threads as accepted findings) or rebuttal (push back with evidence)")
	c.Flags().BoolVar(&auto, "auto", false,
		"arm the completion-gate trigger instead of starting now (one-shot; fires on an open PR)")
	c.Flags().BoolVar(&recheck, "recheck", false,
		"compare the PR's open-thread count against the baseline recorded when the campaign ended, "+
			"and report/emit review_threads_appeared if new findings arrived (arms nothing; needs no --pr)")
	c.Flags().BoolVar(&asJSON, "json", false, "print the raw JSON reply")
	return c
}

// originSlug resolves the CURRENT directory's git origin to owner/name. It is
// the CLI-side twin of the daemon's originRepo, and it is what makes
// `--pushed` mean "the repo I am standing in" rather than "some repo the daemon
// happens to know about".
func originSlug() (owner, repo string, ok bool) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", "", false
	}
	u := strings.TrimSpace(string(out))
	for _, prefix := range []string{
		"git@github.com:", "https://github.com/", "http://github.com/",
		"ssh://git@github.com/",
	} {
		after, cut := strings.CutPrefix(u, prefix)
		if !cut {
			continue
		}
		o, r, split := strings.Cut(strings.TrimSuffix(after, ".git"), "/")
		if split && o != "" && r != "" {
			return o, r, true
		}
	}
	return "", "", false
}

// reviewRecheckCtl sends a repo-wide re-check request and prints the per-job
// verdicts. It goes through the same postReview transport as every other review
// verb; only the fan-out across jobs lives in the daemon (it owns the job list
// and the worktree mapping).
func reviewRecheckCtl(payload map[string]any, asJSON bool) error {
	resp, err := postReview(payload)
	if err != nil {
		return err
	}
	if asJSON {
		out, mErr := json.Marshal(resp)
		if mErr != nil {
			return mErr
		}
		fmt.Println(string(out))
	} else if resp.OK {
		printRecheckVerdicts(resp)
	}
	if !resp.OK {
		return &reviewFailure{Reason: resp.Reason, Message: resp.Error}
	}
	return nil
}

// printRecheckVerdicts prints one line per job so a push hook's output is
// scannable, and names the remedy when threads appeared.
func printRecheckVerdicts(resp control.Response) {
	type entry struct {
		Job          string `json:"job"`
		Checked      bool   `json:"checked"`
		OpenAtClose  int    `json:"open_at_close"`
		OpenNow      int    `json:"open_now"`
		NewThreads   int    `json:"new_threads"`
		Action       string `json:"action"`
		Owner        string `json:"owner"`
		Repo         string `json:"repo"`
		PR           int    `json:"pr"`
		CampaignBusy bool   `json:"campaign_active"`
	}
	// control.Response.Data is already-decoded `any`, so re-marshal rather
	// than unmarshal: the shape is the daemon's, and a re-marshal keeps this
	// printer tolerant of it.
	raw, mErr := json.Marshal(resp.Data)
	var out struct {
		Jobs []entry `json:"jobs"`
	}
	if mErr != nil || json.Unmarshal(raw, &out) != nil || len(out.Jobs) == 0 {
		// No per-job breakdown: fall back to the plain OK printer so the
		// operator still sees whatever the daemon said.
		printReviewOK(resp)
		return
	}
	alerted := 0
	for _, j := range out.Jobs {
		if !j.Checked {
			fmt.Printf("%s: nothing to re-check (%s)\n", j.Job, j.Action)
			continue
		}
		if j.NewThreads > 0 {
			alerted++
			fmt.Printf("%s: %d NEW review thread(s) on %s/%s#%d (%d -> %d open)\n",
				j.Job, j.NewThreads, j.Owner, j.Repo, j.PR, j.OpenAtClose, j.OpenNow)
			fmt.Printf("  %s\n", j.Action)
			continue
		}
		fmt.Printf("%s: %d open thread(s), unchanged since the campaign closed\n", j.Job, j.OpenNow)
	}
	if alerted > 0 {
		fmt.Printf("\n%d job(s) gained review findings after their campaign closed.\n", alerted)
		fmt.Println("Each needs a NEW campaign: pi-supervisor review <job> --pr <N>")
	}
}

// newReviewRecheckCmd implements `pi-supervisor review-recheck`, the entry
// point the post-push hook calls. It re-checks every job whose baseline PR
// belongs to the repository that was just pushed to, so ONE push triggers
// exactly the jobs whose review state could have changed.
//
// This is the push-as-trigger path (ADR-0012 follow-up). It is event-driven:
// git runs the hook, the hook calls the daemon, the daemon compares counts.
// No timer, no polling.
func newReviewRecheckCmd() *cobra.Command {
	var (
		asJSON   bool
		pushed   bool
		repoSlug string
	)
	c := &cobra.Command{
		Use:   "review-recheck",
		Short: "Compare recorded review baselines against the PRs' current open threads",
		Long: "Re-check post-completion review threads.\n\n" +
			"With no flags it re-checks every job that has a recorded review baseline.\n" +
			"--pushed narrows that to the jobs whose PR belongs to the repository\n" +
			"just pushed (the post-push hook's mode), so one push touches only the\n" +
			"jobs whose review state could have changed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := repoSlug
			if pushed && slug == "" {
				// Resolve HERE, in the caller's cwd. Left empty, the daemon
				// falls back to an ARBITRARY job's worktree — which in a
				// multi-repo daemon is the wrong repository, and the re-check
				// then skips every job it should have checked while reporting
				// a confident scope.
				if o, r, ok := originSlug(); ok {
					slug = o + "/" + r
				}
			}
			return reviewRecheckCtl(map[string]any{
				"cmd":    "review_recheck_all",
				"pushed": pushed,
				"repo":   slug,
			}, asJSON)
		},
	}
	c.Flags().BoolVar(&pushed, "pushed", false,
		"only re-check jobs whose review PR belongs to the repository just pushed to")
	c.Flags().StringVar(&repoSlug, "repo", "",
		"restrict --pushed to this owner/name (defaults to this repository's origin)")
	c.Flags().BoolVar(&asJSON, "json", false, "print the raw JSON reply")
	return c
}

// newAckCmd implements `pi-supervisor ack --event <ack_id>` (ADR-0012 §2.3).
//
// Acking is what actually applies a bulk_resolve. It is a separate, explicit
// verb precisely so that nothing closes many threads silently.
func newAckCmd() *cobra.Command {
	var (
		event  string
		asJSON bool
	)
	c := &cobra.Command{
		Use:   "ack <job>",
		Short: "Acknowledge a pending bulk_resolve, applying it",
		Long: "Apply a previously requested bulk resolve. The request stays inert\n" +
			"until it is acked; an unacked request expires and the threads stay open.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			if event == "" {
				return fmt.Errorf("%w: ack requires --event <ack_id>", errUsage)
			}
			return reviewCtl(map[string]any{
				"cmd":   "ack",
				"job":   args[0],
				"event": event,
			}, asJSON)
		},
	}
	c.Flags().StringVar(&event, "event", "", "ack id from bulk_resolve_requested")
	c.Flags().BoolVar(&asJSON, "json", false, "print the raw JSON reply")
	return c
}

// newReviewActionCmd is the shim's own entry point (`_pi-supervisor-review`).
// It is hidden from help because pi calls it through the injected skill, not
// by hand — but it is a real, documented CLI verb.
func newReviewActionCmd() *cobra.Command {
	var (
		jobName string
		pr      int
		round   int
		asJSON  bool
	)
	c := &cobra.Command{
		Use:    "review-action <action>",
		Short:  "Perform one review verb (the shim's transport)",
		Hidden: true,
		Args:   cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			action := args[0]
			body := map[string]any{
				"cmd":    "review_action",
				"job":    jobName,
				"action": action,
				"pr":     pr,
			}
			if round != 0 {
				body["round"] = round
			}
			if len(args) == 2 {
				if err := json.Unmarshal([]byte(args[1]), &body); err != nil {
					return fmt.Errorf("%w: payload is not valid JSON: %w", errUsage, err)
				}
				// The envelope fields are routing, not caller input: they must
				// survive a payload that omits them.
				body["cmd"] = "review_action"
				body["action"] = action
				body["job"] = jobName
			}
			return reviewCtl(body, asJSON)
		},
	}
	c.Flags().StringVar(&jobName, "job", os.Getenv("PI_SUPERVISOR_REVIEW_JOB"),
		"the job whose live round authorizes this call")
	c.Flags().IntVar(&pr, "pr", 0, "pull request number")
	c.Flags().IntVar(&round, "round", 0, "claimed round (checked against the daemon's live round, never trusted)")
	c.Flags().BoolVar(&asJSON, "json", false, "print the raw JSON reply")
	return c
}

// dial opens the control socket, mapping a failure onto the shared
// unreachable sentinel so the exit code stays 2 (ADR-0008).
func dial() (net.Conn, error) {
	c, err := net.Dial("unix", socketPath())
	if err != nil {
		return nil, fmt.Errorf("%w at %s: %w", errUnreachable, socketPath(), err)
	}
	return c, nil
}

func readAll(c net.Conn) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, nil // EOF or peer close: the daemon answers once
		}
		if n == 0 {
			return out, nil
		}
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
