package review

import (
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-github/v90/github"
)

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

// go-github synthesizes a plain error for an unprotected branch instead of
// returning an *ErrorResponse, so the detection must work off the message too.
// Getting this wrong makes every campaign on an unprotected repo treat all
// checks as required (live-test finding).
func TestIsNotProtected(t *testing.T) {
	cases := []struct {
		name string
		err  error
		resp *github.Response
		want bool
	}{
		{"synthesized message", errors.New("branch is not protected"), nil, true},
		{"message casing", errors.New("Branch is not protected"), nil, true},
		{"forbidden is NOT unprotected", errors.New("Resource not accessible by integration"), nil, false},
		{"nil err", nil, nil, false},
	}
	for _, c := range cases {
		if got := isNotProtected(c.err, c.resp); got != c.want {
			t.Errorf("%s: isNotProtected = %v, want %v", c.name, got, c.want)
		}
	}
	// A real *github.ErrorResponse carrying a 404 must also be recognized.
	resp := &http.Response{StatusCode: http.StatusNotFound}
	rerr := &github.ErrorResponse{Response: resp}
	if !isNotProtected(rerr, nil) {
		t.Error("a 404 ErrorResponse must be recognized as not-protected")
	}
	// And a 403 ErrorResponse must NOT be.
	rerr403 := &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}}
	if isNotProtected(rerr403, nil) {
		t.Error("a 403 must not be read as unprotected — that would fail OPEN")
	}
}

// An unprotected branch must NOT be treated as required_unknown: that would make
// every check blocking and deadlock the campaign.
func TestUnprotectedBranchRequiresNothing(t *testing.T) {
	got := rollup("abc", []CICheck{
		{Name: "build", Status: "completed", Conclusion: "failure"},
	}, map[string]bool{}, true)
	got.NoProtection = true
	if got.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass on an unprotected branch", got.Verdict)
	}
	if got.RequiredUnknown {
		t.Error("unprotected must not set RequiredUnknown")
	}
	if len(got.NonBlocking) != 1 {
		t.Errorf("the failing check must be reported as non-blocking: %v", got.NonBlocking)
	}
}

func TestRequiredMatchesIsCaseInsensitive(t *testing.T) {
	req := map[string]bool{"CI / CircleCI": true}
	if !requiredMatches(req, "CI / CircleCI") {
		t.Error("exact match failed")
	}
	if !requiredMatches(req, "ci / circleci") {
		t.Error("case-insensitive match failed — protection contexts and check names differ in case")
	}
	if requiredMatches(req, "Build") {
		t.Error("unrelated check matched a required context")
	}
	if requiredMatches(nil, "Build") {
		t.Error("a nil requirement map must match nothing")
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
