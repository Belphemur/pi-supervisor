package job

import (
	"os"
	"path/filepath"
	"testing"
)

// The report that satisfied the gate while the work was incomplete.
const realPartialReport = `# Mealime user recipes — final report

**Status: PARTIAL.** Tasks T1-T5 are implemented, committed and green. T6-T9 are
NOT done; the T6 design work below is complete and ready to implement.

## What shipped
- T1 recipe_types regeneration
- T2 pin updates

## Repo facts a follow-up run needs
Branch is behind origin/main by 3 commits.

ALL_MEALIME_USERRECIPES_DONE deliberately NOT emitted: T6-T9 are incomplete.
`

func TestReportDeclaresIncompleteOnRealReport(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.md")
	if err := os.WriteFile(p, []byte(realPartialReport), 0o644); err != nil {
		t.Fatal(err)
	}
	phrase, bad := ReportDeclaresIncomplete(p)
	if !bad {
		t.Fatal("the real failing report was NOT detected as partial — the false positive would recur")
	}
	t.Logf("detected: %q", phrase)
}

// A genuinely complete report must NOT be flagged, or the gate would loop forever
// on finished work.
func TestReportCompleteIsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.md")
	body := `# Mealime user recipes — final report

**Status: COMPLETE.** All nine tasks are implemented, committed, and green.

## Verification
- 488 unit tests pass, 0 fail
- Full e2e suite green in CI
- PR #50 merged

ALL_MEALIME_USERRECIPES_DONE
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if phrase, bad := ReportDeclaresIncomplete(p); bad {
		t.Fatalf("a COMPLETE report was flagged as partial: %q", phrase)
	}
}

// A report merely MENTIONING incompleteness in passing prose must not trip it —
// otherwise the gate loops on finished work.
func TestReportMentioningIncompleteInProseIsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.md")
	body := `# Report

The nutrition list was incomplete in the original export, so I regenerated it
from the CIQUAL dataset and re-pinned every reference. Tasks 3-5 are complete.

ALL_MEALIME_USERRECIPES_DONE
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if phrase, bad := ReportDeclaresIncomplete(p); bad {
		t.Fatalf("prose mentioning incompleteness was flagged: %q", phrase)
	}
}

// A missing or empty report is not a partial declaration.
func TestReportMissingIsNotFlagged(t *testing.T) {
	if _, bad := ReportDeclaresIncomplete("/definitely/not/here.md"); bad {
		t.Fatal("missing file flagged")
	}
	if _, bad := ReportDeclaresIncomplete(""); bad {
		t.Fatal("empty path flagged")
	}
}

// qodo PR #5: a COMPLETE report can carry a handoff SECTION titled "Repo facts a
// follow-up run needs" — documentation, not a declaration. It must not divert a
// finished job into another round (that is the same false-direction failure the
// whole gate exists to avoid, mirrored).
func TestReportCompleteWithFollowUpSectionNotFlagged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.md")
	body := `# Task — final report

**Status: COMPLETE.** All tasks implemented and green.

## Repo facts a follow-up run needs
Branch is behind origin/main by 2 commits; the table schema is in doc/adr/.

ALL_TASK_DONE
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if phrase, bad := ReportDeclaresIncomplete(p); bad {
		t.Fatalf("a handoff section in a COMPLETE report was flagged as a partial declaration: %q", phrase)
	}
}

// The DECLARATION form — a follow-up needed TO DO something — still fires, so
// narrowing the pattern did not amputate the real signal. Both present and future
// tense must work; kody flagged that restricting to past-tense "needed" alone
// would let a job whose marker was also emitted close with work unfinished.
func TestReportFollowUpDeclarationStillFires(t *testing.T) {
	for _, body := range []string{
		"A follow-up run is needed to implement T6-T9.\n",
		"A follow-up run will be needed to finish T6-T9.\n",
		"A follow-up run needs to be done: T6-T9 are not implemented.\n",
		"A follow-up run needs to import the new table.\n",
		// A colon value that STARTS with a no-follow-up word but is a real
		// declaration. Matching the marker as a prefix swallowed this and closed
		// the job done.
		"A follow-up run is needed: none of T6-T9 are implemented.\n",
		// Emphasis between the keyword and the colon is how agents bold a
		// declaration; requiring only spaces there dropped it.
		"**Follow-up needed**: T6-T9 are not implemented.\n",
		"**Follow-up required**: finish T6-T9.\n",
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, "r.md")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, bad := ReportDeclaresIncomplete(p); !bad {
			t.Errorf("a genuine follow-up-needed declaration was not detected: %q", body)
		}
	}
}

// Emphasis must not decide the answer, in EITHER direction. Agents bold whatever
// word they feel like bolding, and two review rounds in a row found an emphasis
// placement that one code path handled and another dropped. So the same sentence
// is exercised with the emphasis moved around it, and both outcomes are asserted
// — the parser strips emphasis once, up front, so no pattern can be "the one that
// forgot bolding".
func TestReportFollowUpEmphasisIsIrrelevant(t *testing.T) {
	fires := []string{
		"A follow-up run is needed to implement T6-T9.\n",
		"**A follow-up run is needed to implement T6-T9.**\n",
		"A **follow-up** run is needed to implement T6-T9.\n",
		"A follow-up run is **needed** to implement T6-T9.\n",
		"A follow-up run is needed **to** implement T6-T9.\n",
		"**Follow-up needed**: T6-T9 are not implemented.\n",
		"**Follow-up needed:** T6-T9 are not implemented.\n",
		"__Follow-up needed__: T6-T9 are not implemented.\n",
		"## **Follow-up needed**: T6-T9 are not implemented.\n",
		"### _Follow-up needed_: T6-T9 are not implemented.\n",
		"A follow-up run needs to import the new table.\n",
		"A follow-up run **needs** to import the new table.\n",
		"**Follow-up required**: finish T6-T9.\n",
		"A follow-up run is needed: none of T6-T9 are implemented.\n",
	}
	for _, body := range fires {
		dir := t.TempDir()
		p := filepath.Join(dir, "r.md")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, bad := ReportDeclaresIncomplete(p); !bad {
			t.Errorf("emphasis bypassed a real declaration: %q", body)
		}
	}

	quiet := []string{
		"## Repo facts a follow-up run needs\n",
		"**## Repo facts a follow-up run needs**\n",
		"- **Follow-up needed**: none\n",
		"- Follow-up needed: **none**\n",
		"- Follow-up needed: N/A\n",
		"- Follow-up needed: **N/A**\n",
		"- Follow-up needed: nothing\n",
		"- Follow-up needed: no further action\n",
		"- Follow-up needed: nothing to do\n",
		"- Follow-up needed: none required\n",
		"- Follow-up needed: not needed\n",
		"The next run should rebase onto main.\n",
	}
	for _, body := range quiet {
		dir := t.TempDir()
		p := filepath.Join(dir, "r.md")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if phrase, bad := ReportDeclaresIncomplete(p); bad {
			t.Errorf("emphasis turned a no-action line into a declaration: %q (matched %q)", body, phrase)
		}
	}
}

// The exact ambiguous forms qodo/kody raised, kept as their own test: a heading
// that ENDS in "needs" with body text beneath it, and a lowercase "n/a".
func TestReportAmbiguousFollowUpNotFlagged(t *testing.T) {
	for _, body := range []string{
		// A documentation heading ending in "needs" (qodo PR #5).
		"## Repo facts a follow-up run needs\nSchema lives in doc/adr/.\n",
		// A bullet whose whole point is that there is NOTHING to do.
		"- Follow-up needed: none\n",
		"- Follow-up needed: n/a\n",
		"- Follow-up needed: nothing\n",
		"**Follow-up needed**: none\n",
		// Speculative handoff prose naming no required action.
		"The next run should rebase onto main.\n",
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, "r.md")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if phrase, bad := ReportDeclaresIncomplete(p); bad {
			t.Errorf("a follow-up phrase naming no action was flagged as partial: %q (matched %q)", body, phrase)
		}
	}
}
