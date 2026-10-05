package job

import (
	"os"
	"regexp"
	"strings"
)

// ReportDeclaresIncomplete reports whether a final report announces that the work
// is NOT finished.
//
// This exists because of a real false-positive completion (mealime-userrecipes,
// 2026-10-05). The supervisor reported `state: done`, `marker_found: true`,
// `final_report_exists: true` — while T6-T9 were unimplemented. Two independent
// signals were available and neither was consulted:
//
//	**Status: PARTIAL.** Tasks T1-T5 are implemented, committed and green.
//	T6-T9 are NOT done; the T6 design work below is complete...
//	## Repo facts a follow-up run needs
//	ALL_MEALIME_USERRECIPES_DONE deliberately NOT emitted: T6-T9 are incomplete.
//
// The gate checked only that the report FILE EXISTED. A report that documents
// incomplete work is not evidence of completion — it is evidence of the opposite,
// and an agent that writes one while declining to emit the marker is telling the
// supervisor exactly that.
//
// This is a HEURISTIC, deliberately narrow, because a false positive here is the
// dangerous direction: claiming "done" when the work is partial loses rounds
// silently, whereas claiming "not done" on a genuinely complete run costs one
// extra round and is recoverable. So it fires only on an unambiguous,
// self-declared status — a headline Status/Summary line that says PARTIAL,
// INCOMPLETE, or BLOCKED, or an explicit "not done"/"do not emit the marker".
//
// It returns the matched phrase so the caller can put the agent's own words in
// the diagnostic rather than a generic guess.
func ReportDeclaresIncomplete(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	// Cap the read: this is a status check on a human-scale document, and a
	// runaway report must not be slurped whole to find a status line.
	const maxReportScan = 256 << 10
	if len(data) > maxReportScan {
		data = data[:maxReportScan]
	}
	text := string(data)
	for _, re := range incompleteReportRes {
		if m := re.FindStringSubmatch(text); m != nil {
			return strings.TrimSpace(m[0]), true
		}
	}
	return "", false
}

// incompleteReportRes are deliberately anchored to a STATUS position: a heading
// or a bolded first token, optionally "not"/"is" then the keyword. That keeps a
// sentence merely MENTIONING incompleteness ("the fix for the incomplete list
// was…") from tripping the gate, while catching the way agents actually write
// it.
var incompleteReportRes = []*regexp.Regexp{
	// "**Status: PARTIAL.**", "Status: INCOMPLETE", "## Summary: BLOCKED"
	regexp.MustCompile(`(?im)^[\s>*#-]{0,12}(?:status|summary|overall|result|outcome)\s*[:\-–]\s*\**\s*(?:is\s+)?\**\s*(partial|incomplete|blocked|unfinished|in[ _-]progress)\b`),
	// "**Status: PARTIAL.**" written with the keyword bolded inside a status line
	regexp.MustCompile(`(?im)^[\s>*#-]{0,12}\**\s*(?:status|summary|overall)\s*\**\s*[:\-–]\s*\**\s*(?:is\s+)?\**\s*(?:partial|incomplete|blocked)\b`),
	// "T6-T9 are NOT done", "tasks 3-5 are incomplete"
	regexp.MustCompile(`(?im)^[\s>*#-]{0,12}(?:[\w./-]+\s*(?:[-–]\s*[\w./-]+\s*)?)?(?:are|is|remain|remains)\s+\**\s*(?:not\s+done|not\s+complete|not\s+finished|incomplete|unfinished|outstanding|pending)\b`),
	// "deliberately NOT emitted", "do not emit the marker", "marker NOT emitted"
	regexp.MustCompile(`(?im)\b(?:do\s+not|don't|never)\s+emit\s+(?:the\s+)?marker\b`),
	regexp.MustCompile(`(?im)\bmarker\b[^\n]{0,40}\b(?:not|isn't|wasn't)\s+emitted\b`),
	// "The next run needs", "a follow-up is needed" — BUT only in a status
	// position, like every other pattern here. A handoff SECTION can be titled
	// "Repo facts a follow-up run needs" inside a COMPLETE report (qodo PR #5:
	// that phrase is documentation, not a declaration), and firing on it
	// diverts genuinely finished work into an endless loop.
	regexp.MustCompile(`(?im)^[\s>*#-]{0,12}(?:a\s+)?follow[ -]?up(?:\s+run)?\s+(?:is\s+)?needed\b`),
	regexp.MustCompile(`(?im)^[\s>*#-]{0,12}(?:the\s+)?next\s+run\s+(?:needs?|should)\b`),
}
