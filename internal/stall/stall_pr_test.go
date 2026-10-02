package stall

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PR URL scrape (ADR-0006). The regex is pinned to the REAL signal seen in a
// pi session transcript: `https://github.com/<owner>/<repo>/pull/<number>`.
// The truncated `pull/` fragment (a partially flushed line, or an agent
// still typing) must never match, and the first URL in a chunk wins.
func TestPollPRURL(t *testing.T) {
	const realURL = "https://github.com/Belphemur/XPoint/pull/184"

	tests := []struct {
		name string
		// lines appended to the transcript after New()
		lines []string
		want  string
	}{
		{
			name:  "real-format URL is detected",
			lines: []string{`{"type":"message","role":"assistant","content":"Opened the PR: ` + realURL + `"}`},
			want:  realURL,
		},
		{
			name: "truncated fragment without a number does not match",
			lines: []string{
				`{"content":"opening https://github.com/Belphemur/XPoint/pull/ now"}`,
			},
			want: "",
		},
		{
			name: "no URL at all",
			lines: []string{
				`{"content":"still reviewing threads on the branch"}`,
				`{"content":"https://github.com/Belphemur/XPoint/issues/184 is not a PR"}`,
				`{"content":"https://github.com/Belphemur/XPoint/pull/abc is not a PR"}`,
			},
			want: "",
		},
		{
			name: "first URL wins",
			lines: []string{
				`{"content":"see https://github.com/Belphemur/XPoint/pull/184"}`,
				`{"content":"also https://github.com/Belphemur/other-repo/pull/7"}`,
			},
			want: realURL,
		},
		{
			name:  "truncated-then-real across polls",
			lines: []string{`{"content":"draft https://github.com/Belphemur/XPoint/pull/"}`},
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "session.jsonl")
			if err := os.WriteFile(path, []byte(`{"content":"round 1 opened"}`+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// Long idle window: this test is about the URL scrape, not the
			// stall verdict.
			d := New(path, time.Hour)
			if got := d.PRURL(); got != "" {
				t.Fatalf("PRURL before any content = %q, want empty", got)
			}
			for _, ln := range tc.lines {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString(ln + "\n"); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
				d.Poll()
			}
			if got := d.PRURL(); got != tc.want {
				t.Fatalf("PRURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// A URL that was already in the transcript when the detector started is
// historical (an earlier round's PR): it must not be reported again.
func TestPollPRURLIgnoresHistoricalContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"content":"opened https://github.com/Belphemur/XPoint/pull/184"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, time.Hour)
	d.Poll()
	if got := d.PRURL(); got != "" {
		t.Fatalf("PRURL = %q, want empty (content predates New)", got)
	}
}
