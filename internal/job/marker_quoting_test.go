package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMarkerGateRejectsQuotedMarker is the regression for a real false-positive
// completion (mealime-userrecipes, 2026-10-05).
//
// The supervisor reported `state: done`, `marker_found: true`, with the report
// present — while the work was genuinely incomplete. The agent had written, in an
// assistant message:
//
//	"ALL_MEALIME_USERRECIPES_DONE deliberately NOT emitted: T6-T9 are incomplete"
//
// TranscriptContains is a bare strings.Contains over concatenated assistant text,
// so a marker the agent QUOTED while declining to emit it satisfied the gate. Four
// rounds of real work were silently discarded: a job marked done stops, and the
// next round never runs.
//
// The gate's contract has always been "the marker, in assistant text". It cannot
// mean "anywhere that string appears", because these three cases are
// indistinguishable by substring alone:
//
//   - "ALL_DONE"                      -> emitted
//   - "ALL_DONE deliberately NOT ..."  -> quoted while declining
//   - "the marker is ALL_DONE"         -> explaining the marker
//
// This test pins the behavior that fixes it.
func TestMarkerGateRejectsQuotedMarker(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")

	write := func(text string) {
		line := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":` +
			quote(text) + `}]}}` + "\n"
		if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The false positive: the marker appears, but the sentence declines it.
	write("All work is partial.\n\nALL_MEALIME_USERRECIPES_DONE deliberately **not** emitted: T6-T9 are incomplete.")
	if TranscriptContains(p, "ALL_MEALIME_USERRECIPES_DONE") {
		t.Fatal("FALSE POSITIVE: a quoted, explicitly-declined marker satisfied the gate")
	}

	// The marker embedded in prose must not count either.
	write("Brief says to emit ALL_MEALIME_USERRECIPES_DONE at the end, but I am stopping early.")
	if TranscriptContains(p, "ALL_MEALIME_USERRECIPES_DONE") {
		t.Fatal("FALSE POSITIVE: the marker quoted inside prose satisfied the gate")
	}

	// A real emission still counts — on its own line, which is how every brief
	// instructs the agent to write it.
	write("Pushed and verified.\n\nALL_MEALIME_USERRECIPES_DONE")
	if !TranscriptContains(p, "ALL_MEALIME_USERRECIPES_DONE") {
		t.Fatal("FALSE NEGATIVE: a genuine marker emission was rejected")
	}
}

// quote renders s as a JSON string.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
