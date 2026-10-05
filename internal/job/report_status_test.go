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
