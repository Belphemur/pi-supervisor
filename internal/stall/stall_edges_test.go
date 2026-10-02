package stall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The marker regex: each ADR-0004 phrase must match, and ordinary prose about
// the code must not.
func TestMarkerRegexEdges(t *testing.T) {
	yes := []string{
		"waiting for CI to finish",
		"Waiting on the re-review",
		"gh pr checks",
		"gh pr reviews --json state",
		"gh pr status",
		"gh pr view 42",
		"running /answer-code-review now",
		"kicking off a re-review",
		"checks are still running",
		"CI is pending",
		"stuck in the review loop",
		"waiting on tests",
		"waiting for the build",
	}
	for _, s := range yes {
		if !ciRe.MatchString(s) {
			t.Errorf("marker not detected: %q", s)
		}
	}
	no := []string{
		"I refactored the review module",
		"the CI helper is in lib/",
		"reading the docs about pipelines",
		"tests pass locally",
		"",
		"all checks are green, merging",
		"the workflow is queued", // too generic on its own: would arm on any mention
	}
	for _, s := range no {
		if ciRe.MatchString(s) {
			t.Errorf("false positive: %q", s)
		}
	}
}

// A second park (wake, then park again) counts as another stall: the detector
// re-arms only after new content arrives.
func TestStallReArmsOnSecondPark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, 120*time.Millisecond)
	app := func(line string) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}

	app(`{"content":"waiting for CI"}`)
	if s, _ := d.Poll(); s {
		t.Fatal("marker must only arm")
	}
	time.Sleep(140 * time.Millisecond)
	if s, _ := d.Poll(); !s {
		t.Fatal("first park did not stall")
	}
	// Disarmed: quiet alone is not a stall.
	time.Sleep(140 * time.Millisecond)
	if s, _ := d.Poll(); s {
		t.Fatal("fired again without a new marker")
	}
	// Wake up (progress) then park again.
	app(`{"content":"CI went green, pushing the fix"}`)
	_, _ = d.Poll()
	app(`{"content":"still waiting for CI on the second run"}`)
	_, _ = d.Poll()
	time.Sleep(140 * time.Millisecond)
	s, marker := d.Poll()
	if !s {
		t.Fatal("second park did not stall — detector never re-armed")
	}
	if !strings.Contains(strings.ToLower(marker), "ci") {
		t.Fatalf("marker = %q", marker)
	}
}

// A rotated/truncated transcript re-bases the read offset: without this the
// detector would sit past EOF and never arm again (review fix).
func TestPollRebasesOnRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	big := strings.Repeat("{\"content\":\"filler\"}\n", 500)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, 100*time.Millisecond)

	// pi restarts the file: smaller than the offset we snapshotted.
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Old content is gone; this poll must not read from a stale offset.
	if s, _ := d.Poll(); s {
		t.Fatal("rotated file reported a stall from pre-rotation state")
	}

	// The re-based detector still works: a marker in the new file arms it.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"content":"waiting for CI"}\n`)
	_ = f.Close()
	if s, _ := d.Poll(); s {
		t.Fatal("marker should only arm, not stall")
	}
	time.Sleep(140 * time.Millisecond)
	if s, _ := d.Poll(); !s {
		t.Fatal("detector blind after rotation")
	}
}

// A missing transcript is tolerated: no panic, no stall.
func TestPollMissingFile(t *testing.T) {
	d := New(filepath.Join(t.TempDir(), "gone.jsonl"), time.Second)
	if s, m := d.Poll(); s || m != "" {
		t.Fatalf("missing file = %v/%q, want false/empty", s, m)
	}
}

// New on a missing file starts at offset 0 and arms as soon as it appears.
func TestNewOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "later.jsonl")
	d := New(path, 80*time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"content":"waiting for CI"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.Poll(); s {
		t.Fatal("armed on the first poll")
	}
	time.Sleep(100 * time.Millisecond)
	if s, _ := d.Poll(); !s {
		t.Fatal("marker in a file that appeared after New() never stalled")
	}
}
