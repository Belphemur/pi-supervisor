package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	return h
}

// Per-job file names are load-bearing (invariant 5): external tooling greps
// exactly these paths.
func TestPathsAreLoadBearing(t *testing.T) {
	ctrl, runlog, orch, status := Paths("power-top")
	if ctrl != "/tmp/pi_power-top.ctrl" {
		t.Fatalf("ctrl = %q", ctrl)
	}
	if runlog != "/tmp/pi_power-top_run.log" {
		t.Fatalf("runlog = %q", runlog)
	}
	if orch != "/tmp/pi_power-top_orchestrator.log" {
		t.Fatalf("orch = %q", orch)
	}
	if status != "/tmp/pi_power-top_status.json" {
		t.Fatalf("status = %q", status)
	}
	if Runlog("x") != "/tmp/pi_x_run.log" || Ctrl("x") != "/tmp/pi_x.ctrl" {
		t.Fatal("Runlog/Ctrl disagree with Paths")
	}
}

func TestDirHelpers(t *testing.T) {
	h := tempHome(t)
	if JobsDir() != filepath.Join(h, ".pi", "supervisor", "jobs") {
		t.Fatalf("JobsDir = %q", JobsDir())
	}
	if StateDir() != filepath.Join(h, ".pi", "supervisor", "state") {
		t.Fatalf("StateDir = %q", StateDir())
	}
	if !strings.HasSuffix(PiScripts(), filepath.Join("skills", "autonomous-ai-agents", "pi", "scripts")) {
		t.Fatalf("PiScripts = %q", PiScripts())
	}
	// MungedSessionsDir: leading slash stripped, separators become dashes.
	got := MungedSessionsDir("/home/balor/workspace/eink/xpoint")
	want := filepath.Join(h, ".pi", "agent", "sessions", "--home-balor-workspace-eink-xpoint--")
	if got != want {
		t.Fatalf("MungedSessionsDir = %q, want %q", got, want)
	}
}

// Load applies every documented default and derives the name from the file.
func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "derived-name.json")
	if err := os.WriteFile(path, []byte(`{"brief":"/tmp/b.md"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "derived-name" {
		t.Fatalf("Name = %q, want the file stem", j.Name)
	}
	if j.MaxRounds != 200 || j.TimeoutS != 1800 {
		t.Fatalf("defaults = maxRounds %d timeout %d", j.MaxRounds, j.TimeoutS)
	}
	if j.SessionName != j.Name {
		t.Fatalf("SessionName = %q, want the name", j.SessionName)
	}
	if j.BackoffScale != 1.0 {
		t.Fatalf("BackoffScale = %v, want 1.0", j.BackoffScale)
	}
	if j.CIStallCap != 0 || j.CIStallIdleS != 0 {
		t.Fatal("CI-stall fields should stay zero (0 = daemon default)")
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file: want an error")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("corrupt file: want an error")
	}
}

// Explicit fields win over defaults; BackoffScale <= 0 falls back to 1.0.
func TestLoadExplicitWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.json")
	body := `{"name":"n","max_rounds":6,"timeout_s":120,"session_name":"sn","backoff_scale":0.02}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.MaxRounds != 6 || j.TimeoutS != 120 || j.SessionName != "sn" || j.BackoffScale != 0.02 {
		t.Fatalf("explicit fields lost: %+v", j)
	}

	path2 := filepath.Join(t.TempDir(), "j2.json")
	if err := os.WriteFile(path2, []byte(`{"backoff_scale":-3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	j2, err := Load(path2)
	if err != nil {
		t.Fatal(err)
	}
	if j2.BackoffScale != 1.0 {
		t.Fatalf("negative BackoffScale = %v, want 1.0", j2.BackoffScale)
	}
}

// Save + Load round-trip through the jobs dir.
func TestSaveRoundTrips(t *testing.T) {
	tempHome(t)
	j := Job{Name: "rt", Brief: "/tmp/b.md", Cont: "/tmp/c.txt", Marker: "ALL_DONE",
		Worktree: "/tmp/wt", MaxRounds: 3, CIStallCap: 2, CIStallIdleS: 7, PiBin: "/bin/pi"}
	if err := Save(j); err != nil {
		t.Fatal(err)
	}
	back, err := Load(filepath.Join(JobsDir(), "rt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if back.Marker != "ALL_DONE" || back.CIStallCap != 2 || back.CIStallIdleS != 7 || back.PiBin != "/bin/pi" {
		t.Fatalf("round trip lost fields: %+v", back)
	}
}

func TestStateRoundTripAndMissing(t *testing.T) {
	tempHome(t)
	if err := SaveState("s1", State{Round: 4, State: "running", CIStalls: 2}); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState("s1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Round != 4 || st.State != "running" || st.CIStalls != 2 {
		t.Fatalf("state = %+v", st)
	}
	if _, err := LoadState("absent"); err == nil {
		t.Fatal("missing state: want an error")
	}
}

func TestExistsAndSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if Exists(p) {
		t.Fatal("missing file reported as existing")
	}
	if Size(p) != 0 {
		t.Fatal("missing file size should be 0")
	}
	if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Exists(p) || Size(p) != 5 {
		t.Fatalf("Exists/Size = %v/%d", Exists(p), Size(p))
	}
}

func TestTail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.log")
	if err := os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Newlines collapse to " | " so a diagnostic fits one status line. The
	// file's trailing newline leaves a trailing separator: current behavior,
	// flagged in the review report and deliberately not changed here.
	if got := Tail(p, 100); got != "alpha | beta | gamma |" {
		t.Fatalf("Tail = %q", got)
	}
	// n truncates to the last n BYTES, so a small window is a byte slice,
	// not a line slice.
	if got := Tail(p, 5); got != "amma |" {
		t.Fatalf("small window Tail = %q", got)
	}
	// n <= 0 used to slice out of range.
	if got := Tail(p, 0); got != "" {
		t.Fatalf("Tail(n=0) = %q, want empty", got)
	}
	if got := Tail(p, -1); got != "" {
		t.Fatalf("Tail(n=-1) = %q, want empty", got)
	}
	if got := Tail(filepath.Join(dir, "nope"), 10); got != "" {
		t.Fatalf("missing file Tail = %q", got)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Tail(empty, 10); got != "" {
		t.Fatalf("empty file Tail = %q", got)
	}
}

// A live transcript is appended to while it is read: only newline-terminated
// lines may be returned. The window-start fragment is dropped too.
func TestTailLinesOnlyReturnsCompleteLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "session.jsonl")

	// Torn trailing write (no final newline) is not a line yet.
	if err := os.WriteFile(p, []byte("{\"a\":1}\n{\"b\":2}\n{\"c\":"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := TailLines(p, 10, 300)
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("torn tail not dropped: %q", got)
	}

	// The rest of the write completes the line: now it is returned.
	if err := os.WriteFile(p, []byte("{\"a\":1}\n{\"b\":2}\n{\"c\":3}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = TailLines(p, 10, 300)
	if len(got) != 3 || got[2] != `{"c":3}` {
		t.Fatalf("completed line missing: %q", got)
	}

	// A window that starts mid-line must not surface the fragment.
	var b strings.Builder
	b.WriteString("first\n")
	for range 4000 {
		b.WriteString(strings.Repeat("y", 200))
		b.WriteString("\n")
	}
	b.WriteString("last\n")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got = TailLines(p, 2, 60)
	if len(got) != 2 {
		t.Fatalf("window tail = %q", got)
	}
	if got[1] != "last" {
		t.Fatalf("last = %q, want last", got[1])
	}
	for _, ln := range got {
		if strings.HasPrefix(ln, "y") {
			t.Fatalf("mid-line fragment leaked into the tail: %.40q", ln)
		}
	}
}

// A file that is one torn line (no newline anywhere) yields nothing.
func TestTailLinesTornOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(`{"partial":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TailLines(p, 5, 100); got != nil {
		t.Fatalf("single torn line = %q, want nil", got)
	}
}

func TestRunlogContains(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.log")
	if RunlogContains(p, "ALL_DONE") {
		t.Fatal("missing file should not contain anything")
	}
	if err := os.WriteFile(p, []byte("MARKER ALL_DONE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !RunlogContains(p, "ALL_DONE") {
		t.Fatal("marker in a small file not found")
	}
	// Only the last 4KB window is searched (bash-supervisor parity).
	big := strings.Repeat("z", 5000) + "ALL_DONE"
	if err := os.WriteFile(p, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if !RunlogContains(p, "ALL_DONE") {
		t.Fatal("marker at the end of a big file not found")
	}
	if err := os.WriteFile(p, []byte("ALL_DONE"+strings.Repeat("z", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if RunlogContains(p, "ALL_DONE") {
		t.Fatal("marker outside the 4KB window must not match")
	}
	// An empty marker matches any non-empty file (strings.Contains(x, "") is
	// true). That is exactly why the supervisor's done gate refuses to run
	// without a marker; pinned here so the guard stays necessary.
	if !RunlogContains(p, "") {
		t.Fatal("empty-marker semantics changed: the empty-marker gate guard needs revisiting")
	}
}

func TestLastLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if _, err := LastLines(p, 3); err == nil {
		t.Fatal("missing file: want an error")
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LastLines(p, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty file = %q, want none", got)
	}
	if err := os.WriteFile(p, []byte("1\n2\n3\n4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = LastLines(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "3" || got[1] != "4" {
		t.Fatalf("LastLines(2) = %q", got)
	}
	if got, err = LastLines(p, 99); err != nil || len(got) != 4 {
		t.Fatalf("LastLines(99) = %q (%v)", got, err)
	}
}

// FindSession picks the newest .jsonl in the munged dir, ignoring
// directories and non-jsonl files.
func TestFindSessionNewestWins(t *testing.T) {
	tempHome(t)
	wt := "/home/balor/workspace/eink/wt"
	dir := MungedSessionsDir(wt)
	if err := os.MkdirAll(filepath.Join(dir, "sub.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.jsonl")
	newer := filepath.Join(dir, "new.jsonl")
	for _, p := range []string{old, newer, filepath.Join(dir, "notes.md")} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if got := FindSession("s", wt, time.Time{}); got != newer {
		t.Fatalf("FindSession = %q, want %q", got, newer)
	}
	// A missing session dir is "no session yet", not an error.
	if got := FindSession("s", "/nowhere", time.Time{}); got != "" {
		t.Fatalf("missing dir = %q, want empty", got)
	}
}
