package job

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The watcher tests are hermetic: tempHome(t) points MungedSessionsDir at a
// temp $HOME and the worktree string (which only feeds the munged dir name)
// comes from t.TempDir(). The old hardcoded /home/balor/workspace/eink/wt-*
// paths wrote into the REAL ~/.pi/agent/sessions tree, where leftover
// transcripts from a live campaign could be adopted by the floor-window check
// and make these tests flaky — or pollute a real campaign's discovery.

// TestSessionWatcherAdoptsOnCreate is the event-driven case the watcher
// exists for: the transcript appears AFTER the watcher is armed, and must be
// adopted without waiting for a poll interval.
func TestSessionWatcherAdoptsOnCreate(t *testing.T) {
	tempHome(t)
	wt := t.TempDir()
	dir := MungedSessionsDir(wt)

	w, err := NewSessionWatcher(wt, time.Now())
	if err != nil {
		t.Fatalf("NewSessionWatcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Nothing there yet.
	if got := w.TryPath(); got != "" {
		t.Fatalf("TryPath on empty dir = %q, want empty", got)
	}

	// pi creates its transcript a beat AFTER the launch.
	go func() {
		time.Sleep(120 * time.Millisecond)
		f, err := os.Create(filepath.Join(dir, "live.jsonl"))
		if err == nil {
			_, _ = f.WriteString("{}\n")
			_ = f.Close()
		}
	}()

	got := w.Wait(10 * time.Second)
	if got == "" {
		t.Fatal("Wait timed out; the created transcript was never adopted")
	}
	if filepath.Base(got) != "live.jsonl" {
		t.Fatalf("adopted %q, want live.jsonl", got)
	}
}

// TestSessionWatcherIgnoresStale is the regression guard for the bug that
// motivated this whole change: a transcript that predates the launch must
// never be adopted, even though it is the newest file present.
func TestSessionWatcherIgnoresStale(t *testing.T) {
	tempHome(t)
	wt := t.TempDir()
	dir := MungedSessionsDir(wt)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(dir, "stale.jsonl")
	if err := os.WriteFile(stale, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	w, err := NewSessionWatcher(wt, time.Now())
	if err != nil {
		t.Fatalf("NewSessionWatcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Wait(500 * time.Millisecond); got != "" {
		t.Fatalf("watcher adopted a pre-launch transcript: %q", got)
	}

	// And the polled path agrees with the watched path.
	if got := FindSession("s", wt, time.Now()); got != "" {
		t.Fatalf("FindSession adopted a pre-launch transcript: %q", got)
	}

	// Once the real transcript lands, both pick it.
	fresh := filepath.Join(dir, "fresh.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := w.Wait(10 * time.Second); filepath.Base(got) != "fresh.jsonl" {
		t.Fatalf("after fresh landed: got %q, want fresh.jsonl", got)
	}
}

// TestSessionWatcherIgnoresQuarantine keeps the ADR-0010 guarantee: files
// under _archived-stale/ are never adoption candidates, even when they are the
// newest thing in the tree and fsnotify reports them.
func TestSessionWatcherIgnoresQuarantine(t *testing.T) {
	tempHome(t)
	wt := t.TempDir()
	w, err := NewSessionWatcher(wt, time.Now())
	if err != nil {
		t.Fatalf("NewSessionWatcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	// NewSessionWatcher creates the dir, so the subdir must be made AFTER it
	// is armed — otherwise the watcher's own MkdirAll wipes it.
	arch := filepath.Join(MungedSessionsDir(wt), "_archived-stale")
	if err := os.MkdirAll(arch, 0o755); err != nil {
		t.Fatal(err)
	}

	// A quarantined transcript appears in the SUBDIRECTORY. The watcher is on
	// dir, so it is not even notified; the scan must also reject it.
	if err := os.WriteFile(filepath.Join(arch, "old.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := w.Wait(500 * time.Millisecond); got != "" {
		t.Fatalf("watcher adopted a quarantined transcript: %q", got)
	}
}

// TestSessionWatcherTryPathMatchesFindSession pins the DRY invariant: the
// watched and polled selection rules are the same function, so they can never
// disagree about which file is newest.
func TestSessionWatcherTryPathMatchesFindSession(t *testing.T) {
	tempHome(t)
	wt := t.TempDir()
	dir := MungedSessionsDir(wt)

	w, err := NewSessionWatcher(wt, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("NewSessionWatcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Files are created AFTER the watcher is armed (it owns the dir).
	// Older first, then a newer one.
	for _, n := range []string{"a.jsonl", "b.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	polled := FindSession("s", wt, time.Now().Add(-time.Minute))
	watched := w.TryPath()
	if watched == "" || polled == "" {
		t.Fatalf("both paths must resolve: watched=%q polled=%q", watched, polled)
	}
	if watched != polled {
		t.Fatalf("watched and polled disagree: %q vs %q", watched, polled)
	}
	if filepath.Base(watched) != "b.jsonl" {
		t.Fatalf("newest wins: got %q, want b.jsonl", watched)
	}
}

// TestSessionWatcherCloseIsIdempotent: Close runs from defer plus explicit
// cleanup on several paths, and a double close must not panic.
func TestSessionWatcherCloseIsIdempotent(t *testing.T) {
	tempHome(t)
	w, err := NewSessionWatcher(t.TempDir(), time.Now())
	if err != nil {
		t.Fatalf("NewSessionWatcher: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
