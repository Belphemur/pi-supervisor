package supervisor

// Owner directive 2026-10-08: the review contract skill is injected into
// every campaign round's skill list, deterministically — the wrapper prompt
// carries the obligation, the skill carries the mechanics (verbs, PRRT_ ids,
// reply-before-resolve). A round that must DISCOVER the contract is a round
// that can drift past it (the mealime-presence campaign ran 4 rounds with
// zero shim calls because the job's skills list lacked the skill).

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCampaignSkillsInjectsReviewSkill(t *testing.T) {
	dir := reviewSkillDir()
	if dir == "" {
		t.Skip("no review skill resolvable on this machine (no shim, no pi discovery link)")
	}
	jobSkills := []string{"/home/x/.hermes/skills/software-development/coding-philosophy"}
	got := campaignSkills(jobSkills)
	if len(got) != len(jobSkills)+1 {
		t.Fatalf("campaignSkills = %v, want the job list + the review skill", got)
	}
	if got[len(got)-1] != dir {
		t.Errorf("last skill = %q, want the resolved review skill dir %q", got[len(got)-1], dir)
	}

	// Already listed (same path): no duplicate.
	again := campaignSkills(append(append([]string{}, jobSkills...), dir))
	if len(again) != len(jobSkills)+1 {
		t.Errorf("duplicate injection: %v", again)
	}

	// Already listed under a DIFFERENT path with the same basename (e.g. the
	// pi discovery symlink vs the canonical copy): no duplicate either.
	twin := filepath.Join(filepath.Dir(dir), "..", "other", filepath.Base(dir))
	again2 := campaignSkills(append(append([]string{}, jobSkills...), filepath.Clean(twin)))
	if len(again2) != len(jobSkills)+1 {
		t.Errorf("basename dedup failed: %v", again2)
	}
}

func TestCampaignSkillsWithoutReviewSkillPassesJobListThrough(t *testing.T) {
	// Point the resolver at nothing: hide the shim by name from LookPath is
	// not possible in-process, but the discovery fallback is consulted only
	// after the shim — so this test asserts the passthrough contract when the
	// resolver returns "" by calling the helper with a stubbed dir via an
	// env-independent seam: campaignSkills has no knob, so instead assert
	// reviewSkillDir returns something sane on this machine and that a nil
	// job list plus injection yields exactly one entry.
	dir := reviewSkillDir()
	if dir == "" {
		got := campaignSkills(nil)
		if len(got) != 0 {
			t.Errorf("resolver returned \"\" but campaignSkills injected %v", got)
		}
		return
	}
	got := campaignSkills(nil)
	if len(got) != 1 || got[0] != dir {
		t.Errorf("campaignSkills(nil) = %v, want [%q]", got, dir)
	}
}

func TestReviewSkillDirResolvesCanonicalCopy(t *testing.T) {
	dir := reviewSkillDir()
	if dir == "" {
		t.Skip("no review skill resolvable on this machine")
	}
	if fi, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil || fi.IsDir() {
		t.Fatalf("resolved dir %q has no SKILL.md file: %v", dir, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "_pi-supervisor-review")); err != nil || fi.IsDir() {
		t.Fatalf("resolved dir %q has no shim: %v", dir, err)
	}
}
