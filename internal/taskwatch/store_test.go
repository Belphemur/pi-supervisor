package taskwatch

import (
	"os"
	"path/filepath"
	"testing"
)

// writeStore produces a plugin-shaped tasks file: tmp+rename is honoured as a
// plain write here, because lookup itself only ever opens by NAME.
func writeStore(t *testing.T, path, json string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	return path
}

const goodTask = `{"nextId": 3, "tasks": [{"id": "1", "subject": "Write report",
  "description": "Summarize findings", "status": "completed",
  "activeForm": "Writing report", "owner": "agent",
  "metadata": {"big": "payload"}, "blocks": ["2"], "blockedBy": [],
  "createdAt": 1000, "updatedAt": 2000},
  {"id": "2", "subject": "Review", "description": "", "status": "pending",
  "metadata": {}, "blocks": [], "blockedBy": ["1"], "createdAt": 1000, "updatedAt": 1000}]}`

// ── Resolution parity (ADR-0014 §3) ──

func TestResolvePITasksOverrides(t *testing.T) {
	fixture := t.TempDir()
	cwd := t.TempDir()
	base := Resolver{Env: map[string]string{}, AgentDir: fixture, Cwd: cwd}

	t.Run("off is memory", func(t *testing.T) {
		r := base
		r.Env = map[string]string{"PI_TASKS": "off"}
		got := r.Resolve()
		if !got.Memory || got.Path != "" {
			t.Fatalf("want memory-only, got %+v", got)
		}
	})

	t.Run("absolute path", func(t *testing.T) {
		r := base
		r.Env = map[string]string{"PI_TASKS": "/tmp/elsewhere/tasks.json"}
		got := r.Resolve()
		if got.Path != "/tmp/elsewhere/tasks.json" {
			t.Fatalf("Path = %q", got.Path)
		}
	})

	t.Run("dot path resolves against session cwd", func(t *testing.T) {
		r := base
		r.Env = map[string]string{"PI_TASKS": "./tasks/x.json"}
		got := r.Resolve()
		if got.Path != filepath.Join(cwd, "tasks", "x.json") {
			t.Fatalf("Path = %q, want under the session cwd", got.Path)
		}
		if got.Unavailable {
			t.Fatalf("must resolve, not defer: %+v", got)
		}
	})

	t.Run("dot path without cwd is unavailable, never guessed", func(t *testing.T) {
		r := base
		r.Cwd = ""
		r.Env = map[string]string{"PI_TASKS": "./tasks/x.json"}
		got := r.Resolve()
		if got.Path != "" || !got.Unavailable {
			t.Fatalf("want unavailable, got %+v", got)
		}
	})

	t.Run("named list lands in ~/.pi/tasks", func(t *testing.T) {
		r := base
		r.TasksDir = filepath.Join(fixture, "named")
		r.Env = map[string]string{"PI_TASKS": "shopping"}
		got := r.Resolve()
		want := filepath.Join(r.TasksDir, "shopping.json")
		if got.Path != want {
			t.Fatalf("Path = %q, want %q", got.Path, want)
		}
	})
}

func TestResolveDefaultScopes(t *testing.T) {
	fixture := t.TempDir()
	cwd := t.TempDir()
	sess := "sess-1"
	taskDir := filepath.Join(cwd, ".pi", "tasks")
	base := Resolver{Env: map[string]string{}, AgentDir: fixture, Cwd: cwd, SessionID: sess}

	// setScope resets both config layers: a subtest sees ONLY the scope it
	// declares, because a previously-written project file would otherwise
	// leak into later subtests (project overrides global).
	setScope := func(global, project string) {
		if global != "" {
			writeStore(t, filepath.Join(fixture, "tasks-config.json"), global)
		} else {
			_ = os.Remove(filepath.Join(fixture, "tasks-config.json"))
		}
		if project != "" {
			writeStore(t, filepath.Join(cwd, ".pi", "tasks-config.json"), project)
		} else {
			_ = os.Remove(filepath.Join(cwd, ".pi", "tasks-config.json"))
		}
	}
	setScope("", "")

	t.Run("default session scope", func(t *testing.T) {
		setScope("", "")
		got := base.Resolve()
		if got.Path != filepath.Join(taskDir, "tasks-"+sess+".json") {
			t.Fatalf("Path = %q", got.Path)
		}
		if got.Scope != ScopeSession {
			t.Fatalf("Scope = %q", got.Scope)
		}
	})

	t.Run("project scope from global config", func(t *testing.T) {
		setScope(`{"taskScope": "project"}`, "")
		got := base.Resolve()
		if got.Path != filepath.Join(taskDir, "tasks.json") {
			t.Fatalf("Path = %q", got.Path)
		}
	})

	t.Run("project config overrides global taskScope", func(t *testing.T) {
		setScope(`{"taskScope": "project"}`, `{"taskScope": "session"}`)
		got := base.Resolve()
		if got.Path != filepath.Join(taskDir, "tasks-"+sess+".json") {
			t.Fatalf("Path = %q", got.Path)
		}
	})

	t.Run("memory scope has no file", func(t *testing.T) {
		setScope(`{"taskScope": "memory"}`, "")
		got := base.Resolve()
		if !got.Memory || got.Path != "" {
			t.Fatalf("want memory, got %+v", got)
		}
	})

	t.Run("unknown taskScope is unavailable", func(t *testing.T) {
		setScope(`{"taskScope": "telepathy"}`, "")
		got := base.Resolve()
		if !got.Unavailable {
			t.Fatalf("want unavailable for an unknown scope, got %+v", got)
		}
	})

	t.Run("session-global prefers existing workspace file", func(t *testing.T) {
		setScope(`{"taskScope": "session-global"}`, "")
		staleGlobal := filepath.Join(fixture, "tasks", "sessions", ProjectKey(cwd), "tasks-"+sess+".json")
		writeStore(t, staleGlobal, `{"nextId":1,"tasks":[]}`)
		if got := base.Resolve(); !got.IsGlobal() {
			t.Fatalf("scope = %q", got.Scope)
		}
		// Workspace file wins when it exists.
		ws := filepath.Join(taskDir, "tasks-"+sess+".json")
		writeStore(t, ws, `{"nextId":1,"tasks":[]}`)
		if got := base.Resolve(); got.Path != ws {
			t.Fatalf("session-global must prefer the existing workspace file, got %q", got.Path)
		}
	})

	t.Run("session-global falls back to agent-dir tree", func(t *testing.T) {
		setScope(`{"taskScope": "session-global"}`, "")
		// The workspace file the previous subtest created still exists; a
		// session-global without one must fall back to the agent-dir tree.
		_ = os.Remove(filepath.Join(taskDir, "tasks-"+sess+".json"))
		got := base.Resolve()
		want := filepath.Join(fixture, "tasks", "sessions", ProjectKey(cwd), "tasks-"+sess+".json")
		if got.Path != want {
			t.Fatalf("Path = %q, want %q", got.Path, want)
		}
	})

	t.Run("missing session id cannot resolve a session store", func(t *testing.T) {
		r := base
		r.SessionID = ""
		got := r.Resolve()
		if got.Path != "" || !got.Unavailable {
			t.Fatalf("no session id must fail the lookup, not guess: %+v", got)
		}
	})
}

func TestProjectKeyMatchesPiTasks(t *testing.T) {
	// Expectations mirror pi-tasks' projectKey exactly: resolve(cwd), strip
	// the leading separator, replace / \ : with '-', wrap in --…--. Spaces
	// are NOT encoded. A relative cwd resolves against the process cwd, so
	// it is only asserted through an absolute input in practice.
	cases := map[string]string{
		"/home/user/work/repo":         "--home-user-work-repo--",
		"/":                            "----",
		"/mnt/c:":                      "--mnt-c---",
		"/home/user/space dir/mix:ed/": "--home-user-space dir-mix-ed--",
	}
	for cwd, want := range cases {
		if got := ProjectKey(cwd); got != want {
			t.Errorf("ProjectKey(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestAgentDirEnvOverride(t *testing.T) {
	fixture := t.TempDir()
	// PI_CODING_AGENT_DIR in the CHILD env must win over any constructor
	// default: the agent dir is the child's reality, not the test's.
	cwd := t.TempDir()
	env := map[string]string{"PI_CODING_AGENT_DIR": fixture}
	global := filepath.Join(fixture, "tasks-config.json")
	if err := os.WriteFile(global, []byte(`{"taskScope": "session-global"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Resolver{Env: env, AgentDir: "/should/not/win", Cwd: cwd, SessionID: "s1"}.Resolve()
	want := filepath.Join(fixture, "tasks", "sessions", ProjectKey(cwd), "tasks-s1.json")
	if got.Path != want {
		t.Fatalf("Path = %q, want agent-dir override %q", got.Path, want)
	}
}

// ── Snapshot validation + lookup (ADR-0014 §3, §5) ──

func TestLookupFindsExactTask(t *testing.T) {
	path := writeStore(t, filepath.Join(t.TempDir(), "tasks-1.json"), goodTask)
	info, ok, why := Lookup(path, "1")
	if !ok {
		t.Fatalf("lookup failed: %s", why)
	}
	if info.Subject != "Write report" || info.Description != "Summarize findings" ||
		info.Status != "completed" || info.ActiveForm != "Writing report" ||
		info.Owner != "agent" || info.CreatedAtMS != 1000 || info.UpdatedAtMS != 2000 {
		t.Fatalf("task info wrong: %+v", info)
	}
	if len(info.Blocks) != 1 || info.Blocks[0] != "2" {
		t.Fatalf("blocks = %v", info.Blocks)
	}
	if len(info.BlockedBy) != 0 {
		t.Fatalf("blockedBy = %v", info.BlockedBy)
	}
	// Pending sibling is confirmable only when completed.
	if _, ok, _ := Lookup(path, "2"); ok {
		t.Fatal("pending task must not confirm")
	}
}

func TestLookupFailuresAreSymbolicAndSafe(t *testing.T) {
	dir := t.TempDir()
	t.Run("missing file", func(t *testing.T) {
		_, ok, why := Lookup(filepath.Join(dir, "none.json"), "1")
		if ok || why == "" {
			t.Fatalf("ok=%v why=%q", ok, why)
		}
	})
	t.Run("memory must never guess a file", func(t *testing.T) {
		_, ok, why, tgt := LookupStore("", "1")
		if ok || why == "" || tgt.Path != "" {
			t.Fatalf("memory lookup: ok=%v why=%q tgt=%+v", ok, why, tgt)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		p := writeStore(t, filepath.Join(dir, "bad.json"), "{not json")
		_, ok, why := Lookup(p, "1")
		if ok || why == "" {
			t.Fatalf("bad json: ok=%v why=%q", ok, why)
		}
	})
	t.Run("missing tasks array", func(t *testing.T) {
		p := writeStore(t, filepath.Join(dir, "noarr.json"), `{"nextId": 1}`)
		_, ok, why := Lookup(p, "1")
		if ok || why == "" {
			t.Fatalf("no array: ok=%v why=%q", ok, why)
		}
	})
	t.Run("array instead of object", func(t *testing.T) {
		p := writeStore(t, filepath.Join(dir, "arr.json"), `[1,2,3]`)
		_, ok, why := Lookup(p, "1")
		if ok || why == "" {
			t.Fatalf("array: ok=%v why=%q", ok, why)
		}
	})
	t.Run("duplicate ids are ambiguous", func(t *testing.T) {
		p := writeStore(t, filepath.Join(dir, "dup.json"),
			`{"nextId":2,"tasks":[{"id":"1","subject":"a","description":"","status":"completed","blocks":[],"blockedBy":[],"createdAt":0,"updatedAt":0},{"id":"1","subject":"b","description":"","status":"completed","blocks":[],"blockedBy":[],"createdAt":0,"updatedAt":0}]}`)
		_, ok, why := Lookup(p, "1")
		if ok || why == "" {
			t.Fatalf("duplicates must be ambiguous, got ok=%v why=%q", ok, why)
		}
	})
	t.Run("unknown id stays missing", func(t *testing.T) {
		p := writeStore(t, filepath.Join(dir, "good.json"), goodTask)
		_, ok, why := Lookup(p, "999")
		if ok || why == "" {
			t.Fatalf("unknown id: ok=%v why=%q", ok, why)
		}
	})
	t.Run("empty id is refused", func(t *testing.T) {
		p := writeStore(t, filepath.Join(dir, "good.json"), goodTask)
		_, ok, why := Lookup(p, "")
		if ok || why == "" {
			t.Fatalf("empty id: ok=%v why=%q", ok, why)
		}
	})
}

func TestLookupRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	t.Run("fifo/device never blocks", func(t *testing.T) {
		fifo := filepath.Join(dir, "pipe.json")
		if err := mkfifo(fifo); err != nil {
			t.Skipf("mkfifo unavailable: %v", err)
		}
		_, ok, why := Lookup(fifo, "1")
		if ok || why == "" {
			t.Fatalf("fifo: ok=%v why=%q", ok, why)
		}
	})
	t.Run("oversized file is bounded", func(t *testing.T) {
		p := filepath.Join(dir, "big.json")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(`{"nextId":1,"tasks":[`); err != nil {
			t.Fatal(err)
		}
		padding := make([]byte, maxStoreBytes+1024)
		for i := range padding {
			padding[i] = ' '
		}
		if _, err := f.Write(padding); err != nil {
			t.Fatal(err)
		}
		f.Close()
		_, ok, why := Lookup(p, "1")
		if ok || why == "" {
			t.Fatalf("oversized: ok=%v why=%q", ok, why)
		}
	})
}

func TestLookupIsImmutableAgainstMutation(t *testing.T) {
	dir := t.TempDir()
	p := writeStore(t, filepath.Join(dir, "tasks.json"), goodTask)
	info, ok, why := Lookup(p, "1")
	if !ok {
		t.Fatalf("first lookup failed: %s", why)
	}
	// A later atomic save replaces the file entirely.
	writeStore(t, p, goodTask)
	info.Subject = "tampered"
	info2, ok, _, _ := LookupStore(p, "1")
	if !ok || info2.Subject != "Write report" {
		t.Fatalf("mutation leaked into a later lookup: %+v", info2)
	}
}
