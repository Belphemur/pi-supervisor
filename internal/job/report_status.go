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
// INCOMPLETE, or BLOCKED, an explicit "not done", a "do not emit the marker", or
// a follow-up line that names the work still to be done (followUpDeclaresIncomplete).
//
// It is a VETO on completion, never a proof of it: the answer it returns is only
// ever used to withhold `done`. Every pattern is anchored to a status position so
// ordinary prose that merely mentions incompleteness does not trip it.
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
	return followUpDeclaresIncomplete(text)
}

// followUpDeclaresIncomplete finds a line that declares work still to be done by
// a FOLLOW-UP, and reports it only when the line names what that follow-up must
// do. That action is the discriminator: a follow-up line is a declaration of
// unfinished work when it says what remains, and is just documentation or a
// "nothing to do" note when it does not.
//
//	                                     fires?
//	A follow-up run is needed to implement T6-T9        yes  (names the action)
//	A follow-up run will be needed to finish T6-T9      yes  (present/future/past all count)
//	**Follow-up needed**: T6-T9 are not implemented     yes  (bold is invisible here)
//	## Repo facts a follow-up run needs                 no   (a heading, no action)
//	- Follow-up needed: none                            no   (says there is nothing)
//	The next run should rebase onto main                no   (no action named)
//
// One parser, deliberately: emphasis and the colon VALUE both decide the answer,
// and spreading that across separate regexes is how two review rounds in a row
// found an emphasis form in one path that the other path dropped. Emphasis is
// now removed once, up front, so no pattern can be "the one that forgot bolding".
func followUpDeclaresIncomplete(text string) (string, bool) {
	for line := range strings.Lines(text) {
		// Strip Markdown emphasis so bolding ANYWHERE in the declaration cannot
		// hide it: "**Follow-up needed**:" and "is **needed** to" both reduce to
		// the plain form. This copy is used for MATCHING only — the returned
		// phrase is the original line — so removing `_` cannot corrupt an
		// identifier in the diagnostic (T6_T9 stays as written in the message).
		plain := strings.NewReplacer("*", "", "`", "", "_", "").Replace(line)
		m := followUpLineRe.FindStringSubmatch(strings.TrimSpace(plain))
		if m == nil {
			continue
		}
		// Colon form: the VALUE decides. "needed: none" says there is nothing to
		// do; "needed: none of T6-T9 are implemented" says the opposite. The
		// no-follow-up words must be the whole value — matching them as a prefix
		// swallowed that second form and closed the job as done.
		if value := strings.TrimSpace(m[1]); value != "" && noFollowUpRe.MatchString(value) {
			continue
		}
		return strings.TrimSpace(line), true
	}
	return "", false
}

var (
	// A follow-up declaration on its own line. Group 1 is the colon value, empty
	// for the "… needed to/for <action>" form.
	followUpLineRe = regexp.MustCompile(`(?i)^[>*#-]{0,12}\s*(?:a\s+)?follow[ -]?up(?:\s+run)?\s+(?:(?:is|will\s+be)\s+)?(?:need(?:s|ed)?|required)\s*(?:(?::\s*(.*)$)|(?:[ 	]+(?:to|for)\b))`)
	// The no-follow-up value, as a WHOLE value: "none", "N/A", "nothing",
	// "not needed", or a bare dash.
	// Explicit benign multi-word values are accepted, but no generic prefix:
	// "none of T6-T9 are implemented" must remain an incomplete declaration.
	noFollowUpRe = regexp.MustCompile(`(?i)^(?:none(?:[ 	]+(?:required|needed))?|n/?a|nothing(?:[ 	]+to[ 	]+do)?|no(?:[ 	]+(?:further[ 	]+action|additional[ 	]+work))?|not[ 	]+(?:needed|required))[ 	]*[.!]*$|^[-–—]+$`)
)

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
}
