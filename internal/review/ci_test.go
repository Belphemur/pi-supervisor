package review

import "testing"

// Only REQUIRED checks block. An optional check that fails must not hold a
// review campaign open for its entire budget — that is the livelock the
// required/optional split exists to prevent (ADR-0012 §8, Q8).
func TestRollupOnlyRequiredChecksBlock(t *testing.T) {
	checks := []CICheck{
		{Name: "build", Status: "completed", Conclusion: "success"},
		{Name: "lint", Status: "completed", Conclusion: "failure"},       // optional
		{Name: "spellcheck", Status: "completed", Conclusion: "failure"}, // optional
	}
	req := map[string]bool{"build": true}

	got := rollup("abc123", checks, req, true)
	if got.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass — a failing OPTIONAL check must not block", got.Verdict)
	}
	if len(got.NonBlocking) != 2 {
		t.Errorf("NonBlocking = %d, want the 2 failing optional checks reported", len(got.NonBlocking))
	}
	if len(got.Blocking) != 0 {
		t.Errorf("Blocking = %v, want empty", got.Blocking)
	}
	if got.NonBlockingSummary() == "" {
		t.Error("the optional failures must still be visible to the operator")
	}
}

// A failing REQUIRED check blocks, and names itself.
func TestRollupFailingRequiredBlocks(t *testing.T) {
	checks := []CICheck{
		{Name: "build", Status: "completed", Conclusion: "failure"},
		{Name: "docs", Status: "completed", Conclusion: "success"},
	}
	got := rollup("abc", checks, map[string]bool{"build": true, "docs": true}, true)
	if got.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail", got.Verdict)
	}
	if len(got.Blocking) != 1 || got.Blocking[0].Name != "build" {
		t.Fatalf("Blocking = %v, want just build", got.Blocking)
	}
	if got.BlockingSummary() == "" || got.NonBlockingSummary() != "" {
		t.Errorf("summaries = %q / %q", got.BlockingSummary(), got.NonBlockingSummary())
	}
}

// A still-running REQUIRED check is pending, not passing. Reading it as a pass
// is exactly how a merge ships on a commit whose build never ran.
func TestRollupPendingIsNotPass(t *testing.T) {
	checks := []CICheck{
		{Name: "build", Status: "in_progress", Conclusion: ""},
	}
	got := rollup("abc", checks, map[string]bool{"build": true}, true)
	if got.Verdict != "pending" {
		t.Fatalf("verdict = %q, want pending", got.Verdict)
	}
	if len(got.Blocking) != 1 {
		t.Errorf("a running required check must be listed as blocking: %v", got.Blocking)
	}
}

// A still-running OPTIONAL check must not make the verdict pending either.
func TestRollupRunningOptionalDoesNotBlock(t *testing.T) {
	checks := []CICheck{{Name: "spellcheck", Status: "queued", Conclusion: ""}}
	got := rollup("abc", checks, map[string]bool{"build": true}, true)
	if got.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass — a queued OPTIONAL check must not hold the gate", got.Verdict)
	}
}

// neutral/skipped count as passing (inherited from pre-merge's gate): they are
// deliberate opt-outs, not failures.
func TestRollupNeutralAndSkippedPass(t *testing.T) {
	checks := []CICheck{
		{Name: "codeql", Status: "completed", Conclusion: "neutral"},
		{Name: "wolfram", Status: "completed", Conclusion: "skipped"},
	}
	got := rollup("abc", checks, map[string]bool{"codeql": true, "wolfram": true}, true)
	if got.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass for neutral/skipped", got.Verdict)
	}
}

// When required-ness cannot be determined, fail CLOSED: treat everything as
// required. A spurious "not done" costs rounds; guessing the other way lets a
// red required check through as a pass.
func TestRollupUnknownRequiredFailsClosed(t *testing.T) {
	checks := []CICheck{
		{Name: "mystery", Status: "completed", Conclusion: "failure"},
	}
	got := rollup("abc", checks, nil, false)
	if got.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail — unknown required-ness must fail closed", got.Verdict)
	}
	if !got.RequiredUnknown {
		t.Error("RequiredUnknown must be set so the operator knows why it is strict")
	}
	if !got.All[0].Required {
		t.Error("with unknown required-ness every check must count as required")
	}
}

func TestFailingConclusions(t *testing.T) {
	// GitHub uses the British spelling for this conclusion.
	for _, c := range []string{"failure", "cancelled", "error", "timed_out", "action_required", "startup_failure"} { //nolint:misspell // GitHub API spelling
		if !failing(c) {
			t.Errorf("failing(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"success", "neutral", "skipped", ""} {
		if failing(c) {
			t.Errorf("failing(%q) = true, want false", c)
		}
	}
}

func TestStillRunning(t *testing.T) {
	for _, s := range []string{"", "queued", "in_progress", "waiting", "pending", "requested"} {
		if !stillRunning(s) {
			t.Errorf("stillRunning(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"completed"} {
		if stillRunning(s) {
			t.Errorf("stillRunning(%q) = true, want false", s)
		}
	}
}

// The mixed case that motivated the split: one red required check and one red
// optional check must read as "fail on the required one" and still name both.
func TestRollupMixedRequiredAndOptional(t *testing.T) {
	checks := []CICheck{
		{Name: "build", Status: "completed", Conclusion: "failure"},
		{Name: "spellcheck", Status: "completed", Conclusion: "failure"},
		{Name: "lint", Status: "completed", Conclusion: "success"},
	}
	got := rollup("abc", checks, map[string]bool{"build": true, "lint": true}, true)
	if got.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail", got.Verdict)
	}
	if len(got.Blocking) != 1 || got.Blocking[0].Name != "build" {
		t.Errorf("Blocking = %v, want [build]", got.Blocking)
	}
	if len(got.NonBlocking) != 1 || got.NonBlocking[0].Name != "spellcheck" {
		t.Errorf("NonBlocking = %v, want [spellcheck]", got.NonBlocking)
	}
	if got.BlockingSummary() != "build=failure" {
		t.Errorf("BlockingSummary = %q", got.BlockingSummary())
	}
	if got.NonBlockingSummary() != "spellcheck=failure" {
		t.Errorf("NonBlockingSummary = %q", got.NonBlockingSummary())
	}
}
