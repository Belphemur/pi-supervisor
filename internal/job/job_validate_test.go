package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The live outage this guards against (mem-claims-analysis, 2026-11-03): a
// def written with invented keys was silently normalized to empty strings and
// every round died with "prompt unreadable: empty prompt". Under ADR-0024 the
// same file must FAIL to load, naming the key and suggesting the canonical one.
func TestLoadRejectsKnownSynonymWithHint(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "job.json")
	bad := `{"job":"x","brief_path":"/tmp/b.md","cont_path":"/tmp/c.txt","report":"/tmp/r.md","marker":"DONE"}`
	if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("a def full of invented keys must not load")
	}
	msg := err.Error()
	for _, want := range []string{`"job"`, `"name"`, `"brief_path"`, `"brief"`, `"cont_path"`, `"cont"`, `"report"`, `"final_report"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q lacks %s", msg, want)
		}
	}
}

func TestLoadRejectsUnknownKeyPlain(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "job.json")
	if err := os.WriteFile(p, []byte(`{"name":"x","brief":"/b","cont":"/c","marker":"M","wat":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `"wat"`) {
		t.Fatalf("Load = %v, want an unknown-key error naming \"wat\"", err)
	}
}

// A canonical def — exactly what Save writes — still loads with defaults.
func TestLoadAcceptsCanonicalDef(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(JobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	j := Job{Name: "rt", Brief: "/tmp/b.md", Cont: "/tmp/c.txt", Marker: "ALL_DONE", Worktree: "/tmp/wt"}
	if err := Save(j); err != nil {
		t.Fatal(err)
	}
	got, err := Load(filepath.Join(JobsDir(), "rt.json"))
	if err != nil {
		t.Fatalf("canonical def refused: %v", err)
	}
	if got.MaxRounds != 200 || got.TimeoutS != 1800 || got.SessionName != "rt" || got.BackoffScale != 1.0 {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if err := Validate(got); err != nil {
		t.Fatalf("saved def failed Validate: %v", err)
	}
}

func TestValidateDefects(t *testing.T) {
	base := Job{Name: "x", Brief: "/b", Cont: "/c", Marker: "M", Worktree: "/w", MaxRounds: 1, TimeoutS: 1}
	if err := Validate(base); err != nil {
		t.Fatalf("valid def rejected: %v", err)
	}
	for _, tc := range []struct{ field, mutate string }{
		{"name", "name"},
		{"brief", "brief"},
		{"cont", "cont"},
		{"marker", "marker"},
		{"worktree", "worktree"},
	} {
		j := base
		switch tc.field {
		case "name":
			j.Name = ""
		case "brief":
			j.Brief = ""
		case "cont":
			j.Cont = ""
		case "marker":
			j.Marker = ""
		case "worktree":
			j.Worktree = ""
		}
		err := Validate(j)
		if err == nil || !strings.Contains(err.Error(), tc.mutate) {
			t.Fatalf("Validate(%+v) = %v, want an error naming %s", j, err, tc.field)
		}
	}
	if err := Validate(Job{}); err == nil || !strings.Contains(err.Error(), "max_rounds") {
		t.Fatalf("zero def = %v, want max_rounds named", err)
	}
}

func TestMissingPromptFiles(t *testing.T) {
	dir := t.TempDir()
	brief := filepath.Join(dir, "b.md")
	if err := os.WriteFile(brief, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := Job{Brief: brief, Cont: filepath.Join(dir, "absent.txt")}
	missing := MissingPromptFiles(j)
	if len(missing) != 1 || !strings.Contains(missing[0], "cont=") {
		t.Fatalf("missing = %v, want only cont", missing)
	}
	// final_report is deliberately exempt: the agent writes it during the run.
	j.FinalReport = filepath.Join(dir, "not-yet.md")
	if missing := MissingPromptFiles(j); len(missing) != 1 {
		t.Fatalf("final_report must not be checked: %v", missing)
	}
	// review_brief is checked only when set.
	j.ReviewBrief = filepath.Join(dir, "absent-rb.md")
	if missing := MissingPromptFiles(j); len(missing) != 2 {
		t.Fatalf("set review_brief missing: %v", missing)
	}
}
