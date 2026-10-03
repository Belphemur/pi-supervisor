// Shell-completion support for pi-supervisor. Cobra generates the per-shell
// completion scripts; this file wires the "install into every shell I actually
// use" experience: `pi-supervisor completion install` detects the shells on
// PATH, writes the generated script to that shell's conventional completion
// directory, and adds an idempotent, marker-guarded source line to the rc file
// where the shell needs one (bash/zsh/powershell do; fish auto-loads).
//
// Every path is derived from a base directory (homeDir) so the logic is
// testable against a temp HOME and never touches the real rc files in tests.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// rcMarker guards the injected block so repeated installs are idempotent and
// an operator can find/remove it by grepping for this string.
const (
	rcMarkerStart = "# >>> pi-supervisor completion >>>"
	rcMarkerEnd   = "# <<< pi-supervisor completion <<<"
)

// homeDir returns the base directory for all completion/rc paths. Overridable
// via PI_SUPERVISOR_COMPLETION_HOME so tests (and packagers) can retarget it
// without touching the real home.
func homeDir() string {
	if v := os.Getenv("PI_SUPERVISOR_COMPLETION_HOME"); v != "" {
		return v
	}
	h, _ := os.UserHomeDir()
	return h
}

// shell describes how to install completion for one shell.
type shell struct {
	name string // canonical name used on the CLI: bash|zsh|fish|powershell
	// scriptPath returns where the generated completion script is written.
	scriptPath string
	// rcPath is the startup file that must source the script ("" = the shell
	// auto-loads from scriptPath's directory, so no rc edit is needed).
	rcPath string
	// sourceLines are the guarded lines appended to rcPath. They reference the
	// script via %s (the script path).
	sourceLines []string
}

// rcLines renders the idempotent block for a shell's rc file, referencing the
// completion script path.
func (s shell) rcLines() []string {
	var out []string
	for _, l := range s.sourceLines {
		out = append(out, strings.ReplaceAll(l, "%s", s.scriptPath))
	}
	return out
}

// detectShells returns completion specs for the shells found on PATH. The
// login shell ($SHELL) is always included first when it is one we support, so
// the interactive shell the operator actually uses is covered even if it is
// not otherwise on PATH. Order is stable (login first, then bash, zsh, fish,
// powershell) so output and tests are deterministic.
func detectShells() []shell {
	home := homeDir()
	all := map[string]shell{
		"bash": {
			name:       "bash",
			scriptPath: filepath.Join(home, ".local", "share", "bash-completion", "completions", "pi-supervisor"),
			rcPath:     filepath.Join(home, ".bashrc"),
			// bash-completion auto-loads the directory when installed; the
			// guarded source is a fallback for setups where it is not loaded.
			sourceLines: []string{
				`[ -f "%s" ] && . "%s"`,
			},
		},
		"zsh": {
			name:       "zsh",
			scriptPath: filepath.Join(home, ".zsh", "completions", "_pi-supervisor"),
			rcPath:     filepath.Join(home, ".zshrc"),
			sourceLines: []string{
				"fpath=(" + filepath.Join(home, ".zsh", "completions") + " $fpath)",
				"autoload -Uz compinit && compinit",
			},
		},
		"fish": {
			name:       "fish",
			scriptPath: filepath.Join(home, ".config", "fish", "completions", "pi-supervisor.fish"),
			rcPath:     "", // fish auto-loads ~/.config/fish/completions/*.fish
		},
		"powershell": {
			name:       "powershell",
			scriptPath: filepath.Join(home, ".config", "powershell", "pi-supervisor.ps1"),
			rcPath:     filepath.Join(home, ".config", "powershell", "profile.ps1"),
			sourceLines: []string{
				`if (Test-Path "%s") { . "%s" }`,
			},
		},
	}

	order := []string{"bash", "zsh", "fish", "powershell"}
	var found []shell
	seen := map[string]bool{}
	// Login shell first.
	if base := filepath.Base(os.Getenv("SHELL")); base != "" {
		if s, ok := all[base]; ok {
			found = append(found, s)
			seen[base] = true
		}
	}
	for _, name := range order {
		if seen[name] {
			continue
		}
		if _, err := exec.LookPath(name); err == nil {
			found = append(found, all[name])
			seen[name] = true
		}
	}
	return found
}

// genCompletion writes the shell-appropriate completion script for root into
// sh.scriptPath (creating parent dirs).
func genCompletion(root *cobra.Command, sh shell) error {
	if err := os.MkdirAll(filepath.Dir(sh.scriptPath), 0o755); err != nil {
		return err
	}
	var err error
	switch sh.name {
	case "bash":
		err = root.GenBashCompletionFile(sh.scriptPath)
	case "zsh":
		err = root.GenZshCompletionFile(sh.scriptPath)
	case "fish":
		err = root.GenFishCompletionFile(sh.scriptPath, true /* includeDesc */)
	case "powershell":
		err = root.GenPowerShellCompletionFile(sh.scriptPath)
	default:
		return fmt.Errorf("unsupported shell %q", sh.name)
	}
	if err != nil {
		return err
	}
	return os.Chmod(sh.scriptPath, 0o644)
}

// ensureRC adds the guarded source block to the rc file if it is not already
// present. Returns whether it modified the file. Idempotent: a file already
// containing rcMarkerStart is left untouched.
func ensureRC(sh shell) (bool, error) {
	if sh.rcPath == "" || len(sh.sourceLines) == 0 {
		return false, nil
	}
	existing, err := os.ReadFile(sh.rcPath)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if strings.Contains(string(existing), rcMarkerStart) {
		return false, nil // already installed
	}
	if err := os.MkdirAll(filepath.Dir(sh.rcPath), 0o755); err != nil {
		return false, err
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n" + rcMarkerStart + "\n")
	for _, l := range sh.rcLines() {
		b.WriteString(l + "\n")
	}
	b.WriteString(rcMarkerEnd + "\n")
	if err := os.WriteFile(sh.rcPath, []byte(b.String()), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// installCompletions generates and wires completion for every detected shell.
// It returns one human-readable status line per shell for the caller to print.
func installCompletions(root *cobra.Command) ([]string, error) {
	shells := detectShells()
	if len(shells) == 0 {
		return nil, fmt.Errorf("no supported shell found on PATH (bash/zsh/fish/powershell)")
	}
	var lines []string
	for _, sh := range shells {
		if err := genCompletion(root, sh); err != nil {
			return lines, fmt.Errorf("%s: %w", sh.name, err)
		}
		changed, err := ensureRC(sh)
		if err != nil {
			return lines, fmt.Errorf("%s rc: %w", sh.name, err)
		}
		if sh.rcPath == "" {
			lines = append(lines, fmt.Sprintf("%s: installed %s (auto-loaded)", sh.name, sh.scriptPath))
		} else if changed {
			lines = append(lines, fmt.Sprintf("%s: installed %s + sourced from %s", sh.name, sh.scriptPath, sh.rcPath))
		} else {
			lines = append(lines, fmt.Sprintf("%s: already installed (%s)", sh.name, sh.scriptPath))
		}
	}
	return lines, nil
}

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Install shell completion for every shell detected on PATH",
		Long: "Generates the completion script for each detected shell (bash, zsh,\n" +
			"fish, powershell), writes it to that shell's completion directory,\n" +
			"and adds an idempotent, marker-guarded source line to the rc file\n" +
			"where needed. Re-running is safe.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			lines, err := installCompletions(root)
			for _, l := range lines {
				fmt.Println(l)
			}
			return err
		},
	}
	// `completion` also exposes cobra's per-shell generators (`completion bash`,
	// `completion zsh`, ...) so an operator can print a script to stdout.
	c := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate a completion script (stdout) or install for all shells",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(installCmd)
	// Per-shell generators that print to stdout (mirrors cobra's defaults but
	// under our own `completion` parent).
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		name := sh
		c.AddCommand(&cobra.Command{
			Use:   name,
			Short: "Print the " + name + " completion script to stdout",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				switch name {
				case "bash":
					return root.GenBashCompletion(cmd.OutOrStdout())
				case "zsh":
					return root.GenZshCompletion(cmd.OutOrStdout())
				case "fish":
					return root.GenFishCompletion(cmd.OutOrStdout(), true)
				default:
					return root.GenPowerShellCompletion(cmd.OutOrStdout())
				}
			},
		})
	}
	return c
}
