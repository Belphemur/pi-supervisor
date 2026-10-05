package job

import "testing"

// Refusal shapes found by review after the first fix. Each is a way an agent
// declines the marker that the original patterns missed, and each one reaching the
// gate means a job marked done with work unfinished.
func TestMarkerRefusalShapes(t *testing.T) {
	const marker = "ALL_DONE"
	cases := []struct {
		name string
		text string
		emit bool
	}{
		// The real incident, verbatim in shape.
		{"plain refusal", "ALL_DONE not emitted", false},
		// Markdown emphasis INSIDE the phrase: "**not**" has no whitespace after
		// "not", so a `\s+` requirement silently missed it.
		{"bold refusal", "ALL_DONE **not** emitted: work incomplete", false},
		{"bold not at end", "**ALL_DONE** was **not** emitted", false},
		// The refusal set up on the PREVIOUS line, then the bare marker.
		{"split line", "I will not emit the marker:\nALL_DONE", false},
		{"split line with reason", "I will not emit the marker yet, CI is red:\nALL_DONE", false},
		// Quoting the instruction rather than emitting.
		{"quoting the brief", "the brief says to emit ALL_DONE at the end", false},
		{"conditional", "should emit ALL_DONE only after CI is green", false},
		// Negated completion language.
		{"not done", "T6-T9 are NOT done\nALL_DONE", false},
		// Deliberately NOT emitted, with the word before it.
		{"deliberately", "ALL_DONE deliberately NOT emitted", false},

		// Genuine emissions must all still count.
		{"bare", "ALL_DONE", true},
		{"after prose", "all done\n\nALL_DONE", true},
		{"bold marker", "**ALL_DONE**", true},
		{"heading marker", "## ALL_DONE", true},
		{"sentence then marker", "Done. ALL_DONE", true},
		{"pr link then marker", "pushed\nhttps://github.com/o/r/pull/1\nALL_DONE", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := textEmitsMarker(c.text, marker); got != c.emit {
				t.Fatalf("textEmitsMarker(%q) = %v, want %v", c.text, got, c.emit)
			}
		})
	}
}
