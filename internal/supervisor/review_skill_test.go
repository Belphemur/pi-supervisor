package supervisor

// Owner directive 2026-10-08: the review contract skill is injected into
// every campaign round's skill list, deterministically — the wrapper prompt
// carries the obligation, the skill carries the mechanics (verbs, PRRT_ ids,
// reply-before-resolve). A round that must DISCOVER the contract is a round
// that can drift past it (the mealime-presence campaign ran 4 rounds with
// zero shim calls because the job's skills list lacked the skill).
//
// The tests build a REAL fixture tree (SKILL.md + shim) and resolve through
// it via HOME, so CI exercises the injection without any installed shim or
// discovery link (qodo PR#10 finding 3: skips made the race-test gate
// vacuous).

import (
	"os"
	"path/filepath"
	"testing"
)

// skillFixture writes a minimal but structurally valid review skill dir and
// routes BOTH resolution paths at it: HOME (discovery fallback) and PATH (a
// stub shim whose realpath lands in the fixture — LookPath runs first in the
// resolver, so the installed real shim must be shadowed, not just the
// fallback). Returns the canonical dir.
func skillFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows fallback for os.UserHomeDir
	canonical := filepath.Join(home, "canonical", "pi_supervisor_review")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "SKILL.md"), []byte("---\nname: pi_supervisor_review\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "_pi-supervisor-review"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stub shim on PATH whose EvalSymlinks resolves INSIDE the fixture:
	// resolver path 1 (LookPath) then lands on the fixture like production
	// resolves onto the canonical copy.
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(canonical, "_pi-supervisor-review"), filepath.Join(bin, "_pi-supervisor-review")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return canonical
}

func TestCampaignSkillsInjectsReviewSkill(t *testing.T) {
	canonical := skillFixture(t)

	jobSkills := []string{"/home/x/.hermes/skills/software-development/coding-philosophy"}
	got, injected, failed := campaignSkills(jobSkills)
	if failed {
		t.Fatalf("resolution failed though the fixture exists: %v", got)
	}
	if injected == "" {
		t.Fatalf("nothing injected: %v", got)
	}
	if got[len(got)-1] != canonical {
		t.Errorf("last skill = %q, want the canonical dir %q", got[len(got)-1], canonical)
	}

	// Already listed by the exact canonical path: no duplicate.
	again, injected2, _ := campaignSkills(append(append([]string{}, jobSkills...), canonical))
	if len(again) != len(jobSkills)+1 || injected2 != "" {
		t.Errorf("exact-path dedup failed: %v (injected %q)", again, injected2)
	}

	// Listed via a SYMLINK that resolves to the canonical dir: it IS the
	// contract — no duplicate.
	link := filepath.Join(t.TempDir(), "linkout")
	if err := os.Symlink(canonical, link); err != nil {
		t.Fatal(err)
	}
	viaLink, injected3, _ := campaignSkills(append(append([]string{}, jobSkills...), link))
	if len(viaLink) != len(jobSkills)+1 || injected3 != "" {
		t.Errorf("symlink dedup failed: %v (injected %q)", viaLink, injected3)
	}

	// kody PR#10 round 2: reviewSkillDir's fallback returns the RAW symlink
	// (~/.pi/.../pi_supervisor_review). Simulate it by stubbing the resolver
	// to return the symlink path, with the canonical dir already listed: the
	// injected dir must be normalized before comparison — no duplicate.
	raw := filepath.Join(t.TempDir(), "skills", "pi_supervisor_review")
	if err := os.MkdirAll(filepath.Dir(raw), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonical, raw); err != nil {
		t.Fatal(err)
	}
	prev := reviewSkillDirImpl
	reviewSkillDirImpl = func() string { return raw }
	t.Cleanup(func() { reviewSkillDirImpl = prev })
	viaRaw, injectedRaw, failedRaw := campaignSkills(append(append([]string{}, jobSkills...), canonical))
	if failedRaw || injectedRaw != "" {
		t.Errorf("raw-symlink resolver double-injected: %v (injected %q, failed %t)", viaRaw, injectedRaw, failedRaw)
	}

	// A STALE copy with the same basename that resolves ELSEWHERE is NOT the
	// contract: the canonical dir is still appended (qodo PR#10 finding 1).
	stale := filepath.Join(t.TempDir(), "pi_supervisor_review")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "SKILL.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	withStale, injected4, _ := campaignSkills(append(append([]string{}, jobSkills...), stale))
	if injected4 == "" {
		t.Fatalf("stale same-name copy suppressed the canonical injection: %v", withStale)
	}
	if withStale[len(withStale)-1] != canonical {
		t.Errorf("canonical dir not appended after stale entry: %v", withStale)
	}
}

func TestCampaignSkillsUnresolvableReportsFailure(t *testing.T) {
	// HOME with no shim, no discovery link, and a PATH stripped of the
	// fixture shim: resolution must FAIL loudly (third return) rather than
	// silently degrade (qodo PR#10 finding 2 / kody) — round() turns that
	// into a WARN.
	skillFixture(t)
	t.Setenv("PATH", t.TempDir()) // no _pi-supervisor-review anywhere on it
	got, injected, failed := campaignSkills([]string{"some/other/skill"})
	if !failed {
		t.Fatalf("expected resolveFailed=true with no resolvable skill, got %v", got)
	}
	if injected != "" {
		t.Errorf("injected = %q, want \"\" on failure", injected)
	}
	if len(got) != 1 || got[0] != "some/other/skill" {
		t.Errorf("job list not passed through: %v", got)
	}
}

func TestReviewSkillDirResolvesFixture(t *testing.T) {
	canonical := skillFixture(t)
	dir := reviewSkillDir()
	if dir == "" {
		t.Fatal("reviewSkillDir returned \"\" though the discovery link resolves")
	}
	if dir != canonical {
		t.Errorf("reviewSkillDir = %q, want the canonical %q", dir, canonical)
	}
	if fi, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil || fi.IsDir() {
		t.Fatalf("resolved dir %q has no SKILL.md file: %v", dir, err)
	}
}
