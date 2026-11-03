package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// job create must land a canonical, daemon-loadable def without the operator
// ever typing JSON (ADR-0024).
func TestJobCreateWritesCanonicalDef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	brief := filepath.Join(home, "brief.md")
	cont := filepath.Join(home, "cont.txt")
	if err := os.WriteFile(brief, []byte("do the thing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cont, []byte("continue"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"),
		"job", "create", "demo",
		"--brief", brief, "--cont", cont, "--marker", "DEMO_DONE",
		"--worktree", filepath.Join(home, "wt"),
		"--final-report", filepath.Join(home, "report.md"),
		"--max-rounds", "6", "--timeout-s", "1800",
		"--skills", "/tmp/skillA,/tmp/skillB",
	)
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	data, err := os.ReadFile(filepath.Join(home, ".pi", "supervisor", "jobs", "demo.json"))
	if err != nil {
		t.Fatalf("def not written: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		`"name": "demo"`, `"brief": `, `"cont": `, `"final_report": `,
		`"marker": "DEMO_DONE"`, `"worktree": `, `"max_rounds": 6`,
		`"session_name": "demo"`, "/tmp/skillA", "/tmp/skillB",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("def %s lacks %s", s, want)
		}
	}
	// The invented-key shapes from the outage must not appear anywhere.
	for _, bad := range []string{`"job"`, "brief_path", "cont_path", `"report"`} {
		if strings.Contains(s, bad) {
			t.Fatalf("def contains dropped-key shape %s: %s", bad, s)
		}
	}
	if !strings.Contains(got.stdout, "pi-supervisor reload") || !strings.Contains(got.stdout, "start demo") {
		t.Fatalf("stdout lacks next steps: %q", got.stdout)
	}
}

func TestJobCreateRefusesMissingPromptFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"),
		"job", "create", "demo",
		"--brief", filepath.Join(home, "absent.md"), "--cont", filepath.Join(home, "absent.txt"),
		"--marker", "M", "--worktree", filepath.Join(home, "wt"),
	)
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d", got.code, exitUsage)
	}
	if !strings.Contains(got.stderr, "do not exist") {
		t.Fatalf("stderr = %q, want the missing-files reason", got.stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".pi", "supervisor", "jobs", "demo.json")); !os.IsNotExist(err) {
		t.Fatal("a rejected create must not write the def")
	}
}

func TestJobCreateRequiresFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), "job", "create", "demo")
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d", got.code, exitUsage)
	}
	if !strings.Contains(got.stderr, "brief") {
		t.Fatalf("stderr = %q, want the required-flag message", got.stderr)
	}
}

func TestJobCreateRefusesOverwriteWithoutForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	brief := filepath.Join(home, "brief.md")
	if err := os.WriteFile(brief, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"job", "create", "demo", "--brief", brief, "--cont", brief,
		"--marker", "M", "--worktree", filepath.Join(home, "wt")}
	if got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), args...); got.code != exitOK {
		t.Fatalf("first create exit = %d, stderr = %s", got.code, got.stderr)
	}
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), args...)
	if got.code != exitUsage || !strings.Contains(got.stderr, "--force") {
		t.Fatalf("overwrite exit = %d stderr = %q, want a --force refusal", got.code, got.stderr)
	}
	if got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), append(args, "--force")...); got.code != exitOK {
		t.Fatalf("--force exit = %d, stderr = %s", got.code, got.stderr)
	}
}

// job list surfaces an invalid def instead of hiding it (ADR-0024).
func TestJobListShowsInvalidDef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	jobsDir := filepath.Join(home, ".pi", "supervisor", "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The mem-claims-analysis shape: invented keys only.
	if err := os.WriteFile(filepath.Join(jobsDir, "memclaims.json"),
		[]byte(`{"job":"memclaims","brief_path":"/b","cont_path":"/c","report":"/r"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), "job", "list")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "INVALID") || !strings.Contains(got.stdout, `did you mean "name"`) {
		t.Fatalf("stdout = %q, want the invalid def named with the synonym hint", got.stdout)
	}
}

func TestJobCommandsListedInHelp(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Name() != "job" {
			continue
		}
		var create, list, show bool
		for _, sub := range c.Commands() {
			switch sub.Name() {
			case "create":
				create = true
				if sub.Flags().Lookup("marker") == nil || sub.Flags().Lookup("worktree") == nil {
					t.Fatal("job create is missing its required flags")
				}
			case "list":
				list = true
			case "show":
				show = true
			}
		}
		if !create || !list || !show {
			t.Fatalf("job subcommands create/list/show missing (create=%v list=%v show=%v)", create, list, show)
		}
		return
	}
	t.Fatal("job command not registered on the root")
}
