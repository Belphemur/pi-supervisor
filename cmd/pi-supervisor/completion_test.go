package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// completionTestRoot builds a throwaway command tree so the generators produce
// real scripts without touching the installed pi-supervisor root.
func completionTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "pi-supervisor", Short: "test"}
	root.AddCommand(&cobra.Command{Use: "status", Short: "status"})
	return root
}

// Every supported shell: the generated script exists, is non-trivial, and (for
// rc-backed shells) the guarded block is written.
func TestCompletionInstallPerShell(t *testing.T) {
	cases := map[string]shell{
		"bash":       {name: "bash", sourceLines: []string{`[ -f "%s" ] && . "%s"`}},
		"zsh":        {name: "zsh", sourceLines: []string{"fpath=(x $fpath)", "autoload -Uz compinit && compinit"}},
		"fish":       {name: "fish"},
		"powershell": {name: "powershell", sourceLines: []string{`if (Test-Path "%s") { . "%s" }`}},
	}
	for name, sh := range cases {
		t.Run(name, func(t *testing.T) {
			sh := sh
			home := t.TempDir()
			t.Setenv("PI_SUPERVISOR_COMPLETION_HOME", home)
			sh.scriptPath = filepath.Join(home, "completions", "pi-supervisor")
			if sh.rcPath == "" {
				sh.rcPath = ""
			} else {
				sh.rcPath = filepath.Join(home, "rcfile")
			}
			if err := genCompletion(completionTestRoot(), sh); err != nil {
				t.Fatalf("gen: %v", err)
			}
			data, err := os.ReadFile(sh.scriptPath)
			if err != nil {
				t.Fatalf("script not written: %v", err)
			}
			if len(data) < 50 {
				t.Fatalf("generated script suspiciously short (%d bytes)", len(data))
			}
			if !strings.Contains(string(data), "pi-supervisor") {
				t.Fatal("generated script does not mention the binary")
			}
			changed, err := ensureRC(sh)
			if err != nil {
				t.Fatalf("ensureRC: %v", err)
			}
			if sh.rcPath == "" {
				// fish auto-loads: no rc edit expected.
				if changed {
					t.Fatal("fish must not edit an rc file")
				}
				return
			}
			if !changed {
				t.Fatal("ensureRC must report it added the block")
			}
			rc, err := os.ReadFile(sh.rcPath)
			if err != nil {
				t.Fatalf("rc not written: %v", err)
			}
			for _, marker := range []string{rcMarkerStart, rcMarkerEnd} {
				if !strings.Contains(string(rc), marker) {
					t.Fatalf("rc missing %s: %q", marker, string(rc))
				}
			}
			if strings.Contains(string(rc), "%s") {
				t.Fatalf("source line kept an unsubstituted %%s: %q", string(rc))
			}
		})
	}
}

// Idempotent: a second install must not duplicate the guarded block.
func TestEnsureRCIsIdempotent(t *testing.T) {
	home := t.TempDir()
	sh := shell{
		name:        "bash",
		scriptPath:  filepath.Join(home, "c"),
		rcPath:      filepath.Join(home, "rc"),
		sourceLines: []string{`[ -f "%s" ] && . "%s"`},
	}
	if _, err := ensureRC(sh); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(sh.rcPath)
	if changed, err := ensureRC(sh); err != nil || changed {
		t.Fatalf("second ensureRC: changed=%v err=%v, want no change", changed, err)
	}
	second, _ := os.ReadFile(sh.rcPath)
	if string(first) != string(second) {
		t.Fatalf("rc changed on reinstall:\n%q\n%q", string(first), string(second))
	}
	if n := strings.Count(string(second), rcMarkerStart); n != 1 {
		t.Fatalf("marker appears %d times, want 1", n)
	}
}

// An existing rc file without a trailing newline is preserved (we append a
// newline rather than gluing onto the last line).
func TestEnsureRCAppendsCleanly(t *testing.T) {
	home := t.TempDir()
	rcPath := filepath.Join(home, "rc")
	if err := os.WriteFile(rcPath, []byte("export FOO=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh := shell{name: "zsh", rcPath: rcPath, scriptPath: filepath.Join(home, "c"),
		sourceLines: []string{"autoload -Uz compinit"}}
	if _, err := ensureRC(sh); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(rcPath)
	if !strings.Contains(string(got), "export FOO=1\n") {
		t.Fatalf("existing content not preserved on its own line: %q", string(got))
	}
}

// detectShells never errors and returns only known shells; it includes $SHELL
// even when that shell is otherwise not on PATH.
func TestDetectShellsIncludesLoginShell(t *testing.T) {
	t.Setenv("PI_SUPERVISOR_COMPLETION_HOME", t.TempDir())
	t.Setenv("SHELL", "/weird/path/to/fish")
	shells := detectShells()
	if len(shells) == 0 {
		t.Skip("no supported shell on PATH and SHELL is unsupported; nothing to assert")
	}
	found := false
	known := map[string]bool{"bash": true, "zsh": true, "fish": true, "powershell": true}
	for _, sh := range shells {
		if !known[sh.name] {
			t.Fatalf("unknown shell %q from detection", sh.name)
		}
		if sh.name == "fish" {
			found = true
		}
	}
	if !found {
		t.Fatal("$SHELL=fish must be detected even when fish is not on PATH")
	}
}
