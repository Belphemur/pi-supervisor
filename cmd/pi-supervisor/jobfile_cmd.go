// `pi-supervisor job` — write and inspect job definitions without
// hand-authoring JSON (ADR-0024). The silent-drop class that killed
// mem-claims-analysis (invented keys like `brief_path` normalized to empty
// strings, then every round died with "prompt unreadable: empty prompt")
// cannot be constructed through this surface: the flags are typed, the
// required ones are enforced by cobra, and the def is validated — structurally
// and for on-disk prompt files — before anything is written.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"pi-supervisor/internal/job"
)

func newJobDefCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "job",
		Short: "Manage job definitions (create/list/show)",
		Long: "Manage job definitions in ~/.pi/supervisor/jobs/ WITHOUT hand-writing JSON\n" +
			"(ADR-0024). Every field is a typed flag with its real name — there are no\n" +
			"invented keys to silently drop. `create` validates the def (required\n" +
			"fields, prompt files on disk) and refuses to overwrite without --force.\n" +
			"An unknown key can no longer be written at all; hand-edited files are\n" +
			"still checked strictly by the daemon at reload.\n\n" +
			"After creating a def: `pi-supervisor reload && pi-supervisor start <name>`.",
	}
	cmd.AddCommand(newJobCreateCmd(), newJobListCmd(), newJobShowCmd())
	return cmd
}

func newJobCreateCmd() *cobra.Command {
	var (
		brief, cont, marker, worktree string
		finalReport, sessionName      string
		skillsCSV, piBin              string
		provider, model               string
		reviewBrief                   string
		maxRounds, timeoutS           int
		ciStallCap, ciStallIdleS      int
		emptyTurnIdleS                int
		backoffScale                  float64
		force                         bool
	)
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Write a new job definition (<name>.json) from typed flags",
		Long: "Write ~/.pi/supervisor/jobs/<name>.json from typed flags and validate it\n" +
			"before anything lands on disk (ADR-0024).\n\n" +
			"Required: --brief, --cont, --marker, --worktree.\n" +
			"The brief/cont files must already exist (final_report is exempt: the\n" +
			"agent writes it during the run).\n\n" +
			"Example:\n" +
			"  pi-supervisor job create mem-claims-analysis3 \\\n" +
			"    --brief /tmp/pi_memclaims_brief.md \\\n" +
			"    --cont /tmp/pi_memclaims_cont.txt \\\n" +
			"    --marker MEM_CLAIMS_ANALYSIS_DONE \\\n" +
			"    --worktree /home/balor/workspace/eink/crosspoint-x-reader/_worktrees/mem-claims \\\n" +
			"    --final-report /tmp/pi_memclaims_report.md \\\n" +
			"    --max-rounds 6 --timeout-s 1800",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
				return fmt.Errorf("%w: job name must be a plain file name, got %q", errUsage, name)
			}
			j := job.Job{
				Name:           name,
				Brief:          brief,
				Cont:           cont,
				FinalReport:    finalReport,
				Marker:         marker,
				Worktree:       worktree,
				SessionName:    sessionName,
				ReviewBrief:    reviewBrief,
				MaxRounds:      maxRounds,
				TimeoutS:       timeoutS,
				PiBin:          piBin,
				Provider:       provider,
				Model:          model,
				BackoffScale:   backoffScale,
				CIStallCap:     ciStallCap,
				CIStallIdleS:   ciStallIdleS,
				EmptyTurnIdleS: emptyTurnIdleS,
			}
			if skillsCSV != "" {
				for s := range strings.SplitSeq(skillsCSV, ",") {
					if s = strings.TrimSpace(s); s != "" {
						j.Skills = append(j.Skills, s)
					}
				}
			}
			if j.SessionName == "" {
				j.SessionName = name
			}
			// Structural gate first (same Validate the daemon runs at reload),
			// then the on-disk prompt files.
			if err := job.Validate(j); err != nil {
				return fmt.Errorf("%w: %w", errUsage, err)
			}
			if missing := job.MissingPromptFiles(j); len(missing) > 0 {
				return fmt.Errorf("%w: prompt files do not exist: %s (create them first; the first round reads the brief verbatim)", errUsage, strings.Join(missing, ", "))
			}
			dst := filepath.Join(job.JobsDir(), name+".json")
			if _, err := os.Stat(dst); err == nil && !force {
				return fmt.Errorf("%w: %s already exists (use --force to overwrite)", errUsage, dst)
			}
			if err := job.Save(j); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "job def written: %s\n", dst)
			fmt.Fprintf(out, "next: pi-supervisor reload && pi-supervisor start %s\n", name)
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&brief, "brief", "", "path to the entry prompt file (round 1 LAUNCH reads it) [required]")
	f.StringVar(&cont, "cont", "", "path to the continuation prompt file (every resume round reads it) [required]")
	f.StringVar(&marker, "marker", "", "completion marker the agent emits as assistant text when the work is done [required]")
	f.StringVar(&worktree, "worktree", "", "worktree the pi session runs in (session discovery is cwd-keyed) [required]")
	f.StringVar(&finalReport, "final-report", "", "path the agent writes its final report to (completes the job with the marker)")
	f.StringVar(&sessionName, "session-name", "", "pi session name (default: the job name)")
	f.StringVar(&skillsCSV, "skills", "", "comma-separated skill paths passed as --skill flags")
	f.StringVar(&piBin, "pi-bin", "", "pi executable (default: resolve \"pi\" on PATH)")
	f.StringVar(&provider, "provider", "", "pin pi's provider for this job (a request, not a guarantee)")
	f.StringVar(&model, "model", "", "pin pi's model for this job (a request, not a guarantee)")
	f.StringVar(&reviewBrief, "review-brief", "", "entry prompt for review campaigns (a campaign then LAUNCHes its own fresh session, ADR-0018)")
	f.IntVar(&maxRounds, "max-rounds", 200, "round cap before the job goes fatal")
	f.IntVar(&timeoutS, "timeout-s", 1800, "per-round client timeout in seconds")
	f.IntVar(&ciStallCap, "ci-stall-cap", 0, "CI-stall retries before the finish-the-report interrupt (0 = default 3)")
	f.IntVar(&ciStallIdleS, "ci-stall-idle-s", 0, "quiet window that arms a CI stall (0 = default 300)")
	f.IntVar(&emptyTurnIdleS, "empty-turn-idle-s", 0, "quiet window for the empty-turn stall (0 = default 60; raise for jobs that wait on child builds)")
	f.Float64Var(&backoffScale, "backoff-scale", 0, "multiplier on every inter-round backoff (0 = 1.0)")
	f.BoolVar(&force, "force", false, "overwrite an existing def instead of refusing")
	for _, req := range []string{"brief", "cont", "marker", "worktree"} {
		_ = c.MarkFlagRequired(req)
	}
	return c
}

func newJobListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List job definitions with their key fields",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := os.ReadDir(job.JobsDir())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				j, lerr := job.Load(filepath.Join(job.JobsDir(), e.Name()))
				if lerr == nil {
					lerr = job.Validate(j)
				}
				if lerr != nil {
					fmt.Fprintf(out, "%-28s INVALID: %v\n", strings.TrimSuffix(e.Name(), ".json"), lerr)
					continue
				}
				fmt.Fprintf(out, "%-28s marker=%s max_rounds=%d timeout=%ds worktree=%s\n",
					j.Name, j.Marker, j.MaxRounds, j.TimeoutS, j.Worktree)
			}
			return nil
		},
	}
}

func newJobShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "show <job>",
		Short:             "Print one job definition as normalized JSON",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			j, err := job.Load(filepath.Join(job.JobsDir(), args[0]+".json"))
			if err != nil {
				return err
			}
			data, err := json.MarshalIndent(j, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return nil
		},
	}
}
