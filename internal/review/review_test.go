package review

import "testing"

// The thread-id rule is the single most load-bearing invariant on this
// surface: a COMMENT node id is silently rejected by both mutations, and an
// LLM handed that field will use it.
func TestValidThreadID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"PRRT_kwDOUDrzps6cpWH0", true},
		{"PRRT_", true}, // prefix check only; the API rejects a truncated id
		{"3904873498", false},
		{"3867470939", false},
		{"", false},
		{"IC_kwDO", false}, // review comment node, not a thread
		{"prrt_lower", false},
	}
	for _, c := range cases {
		if got := ValidThreadID(c.id); got != c.want {
			t.Errorf("ValidThreadID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// CleanBody is inherited from the retired reply_review.py helper: an
// un-cleaned CodeRabbit body is mostly analysis-chain noise, so the agent
// triages noise unless the markup is stripped.
func TestCleanBody(t *testing.T) {
	raw := `_🩺 Stability_ | _🟠 Major_ | _🏗️ Heavy lift_

Here is the real recommendation.

<details>🧩 Analysis chain</details>
some script output

<!-- hidden -->

` + "```cpp\nint x;\n```" + `

trailing`
	got := CleanBody(raw)
	if contains(got, "Analysis chain") {
		t.Error("analysis chain survived cleaning")
	}
	if contains(got, "int x;") {
		t.Error("fenced code block survived cleaning")
	}
	if contains(got, "hidden") {
		t.Error("HTML comment survived cleaning")
	}
	if !contains(got, "real recommendation") {
		t.Errorf("the actual finding was stripped: %q", got)
	}
	if CleanBody("") != "" {
		t.Error("empty body must clean to empty")
	}
}

func contains(hay, needle string) bool {
	return len(needle) > 0 && len(hay) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(hay); i++ {
				if hay[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}

// The exit class is how an LLM decides retry-vs-change-the-call. It must live
// on the reason, never be inferred from prose.
func TestReasonExitCode(t *testing.T) {
	two := []Reason{
		ReasonUsage, ReasonNoLiveRound, ReasonRoundMismatch,
		ReasonNotAnswered, ReasonUnknownThread,
	}
	for _, r := range two {
		if got := r.ExitCode(); got != 2 {
			t.Errorf("%s.ExitCode() = %d, want 2 (the caller must change the call)", r, got)
		}
	}
	one := []Reason{ReasonAuthUnavailable, ReasonRateLimited, ReasonGitHubError}
	for _, r := range one {
		if got := r.ExitCode(); got != 1 {
			t.Errorf("%s.ExitCode() = %d, want 1 (the caller may retry)", r, got)
		}
	}
}

func TestOpenThreadsFiltersResolved(t *testing.T) {
	all := []Thread{
		{ThreadID: "PRRT_a", Resolved: false},
		{ThreadID: "PRRT_b", Resolved: true},
		{ThreadID: "PRRT_c", Resolved: false},
	}
	open := OpenThreads(all)
	if len(open) != 2 {
		t.Fatalf("OpenThreads = %d, want 2", len(open))
	}
	for _, o := range open {
		if o.ThreadID == "PRRT_b" {
			t.Error("a resolved thread leaked into the open set")
		}
	}
}

func TestParsePRURL(t *testing.T) {
	cases := []struct {
		in     string
		owner  string
		repo   string
		pr     int
		wantOK bool
	}{
		{"https://github.com/Belphemur/crosspoint-x-reader/pull/184", "Belphemur", "crosspoint-x-reader", 184, true},
		{"http://github.com/o/r/pull/7", "o", "r", 7, true},
		{"https://github.com/o/r/pull/", "", "", 0, false},
		{"https://github.com/o/r/issues/7", "", "", 0, false},
		{"not a url", "", "", 0, false},
		{"", "", "", 0, false},
	}
	for _, c := range cases {
		o, rp, n, ok := ParsePRURL(c.in)
		if ok != c.wantOK {
			t.Errorf("ParsePRURL(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if o != c.owner || rp != c.repo || n != c.pr {
			t.Errorf("ParsePRURL(%q) = %s/%s#%d, want %s/%s#%d",
				c.in, o, rp, n, c.owner, c.repo, c.pr)
		}
	}
}

// A truncated pull URL must never yield a PR number of 0 or a bogus value:
// the auto-trigger derives its whole target from this parse.
func TestParsePRURLRejectsPartial(t *testing.T) {
	if _, _, n, ok := ParsePRURL("https://github.com/o/r/pull/"); ok || n != 0 {
		t.Errorf("truncated pull URL parsed as ok=%v n=%d", ok, n)
	}
}
