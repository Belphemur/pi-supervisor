package job

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// stemOverlap reports whether two report filenames plausibly name the same
// artifact, ignoring (a) the extension and (b) a trailing digit run.
//
// The digit run is the whole point. Job names get versioned —
// "mealime-extracats" becomes "mealime-extracats3" — and when the brief and the
// job's final_report disagree, they usually differ ONLY by that suffix. So
// "mealime_extracats_final_report.md" and "mealime_extracats3_final_report.md"
// must be recognized as the same report.
//
// It stays deliberately narrow: one stem must be a prefix of the other after
// the digit run is trimmed, so two genuinely different reports ("alpha_report"
// vs "beta_report") never match, and neither does a file that merely shares the
// word "report".
func stemOverlap(a, b string) bool {
	sa := stem(a)
	sb := stem(b)
	if sa == "" || sb == "" {
		return false
	}
	ta := versionless(sa)
	tb := versionless(sb)
	if ta == "" || tb == "" {
		return false
	}
	// Identical once version digits are removed: the same report at a
	// different version.
	if ta == tb {
		return true
	}
	// Or one is a prefix of the other, which covers ".round1"-style suffixes
	// and longer descriptive variants.
	return strings.HasPrefix(ta, tb) || strings.HasPrefix(tb, ta)
}

// versionless removes digit runs from a stem, wherever they appear.
//
// NOT a trailing trim: the real disagreement is "mealime_extracats3_final_report"
// vs "mealime_extracats_final_report", where the "3" sits BEFORE
// "_final_report", and "run_report.round2" vs "run_final_report", where it sits
// after a dot. A TrimRight only handles the trailing case and misses both.
//
// Every maximal run of digits is REMOVED, so both "task3_report" and
// "task_report" collapse to "task_report".
//
// Removing beats replacing with a separator: a separator leaves "task__report"
// (doubled underscore) or "report_" (trailing underscore), neither of which
// matches the other side. Removal makes the comparison order-independent.
func versionless(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// stem is a lowercased filename without its extension.
func stem(name string) string {
	base := filepath.Base(name)
	return strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
}

// FindReportNearby looks for a final report that EXISTS but is not at the
// configured path, and returns it.
//
// Why this exists: `done` requires the marker AND the final report (ADR-0011).
// When a brief and the job's `final_report` disagree on the filename — a very
// easy slip, since the two are written separately and the marker still fires —
// the agent dutifully writes the file the brief named, the gate checks the path
// the job names, and the job burns every remaining round re-running finished
// work. That happened live on mealime-extracats3: 13 rounds, `marker_seen=true`,
// and last_diag "round cap reached without marker" pointing at the wrong cause.
//
// This turns that diagnosis into a lookup. It is deliberately CONSERVATIVE —
// it only proposes a file, it never moves or creates one, because the remedy
// (copying the agent's report to the configured path) is the operator's call.
//
// Matching rules, in order of confidence:
//   - same directory as the configured path, and
//   - filename contains the configured base name or vice versa (so
//     "mealime_extracats_final_report.md" matches a configured
//     "mealime_extracats3_final_report.md"), and
//   - non-empty, and
//   - modified after the job started (so a stale report from a previous run is
//     never proposed), and
//   - newest first, so the best candidate is the answer.
//
// Returns "" when nothing plausible is found. Ambiguity is NOT an error: the
// newest plausible file wins, and the caller reports the path so a human can
// sanity-check it.
func FindReportNearby(configured string, notBefore time.Time) string {
	if configured == "" {
		return ""
	}
	dir := filepath.Dir(configured)
	base := strings.ToLower(filepath.Base(configured))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// The configured path itself is not a "nearby" file.
		if filepath.Join(dir, name) == configured {
			continue
		}
		lower := strings.ToLower(name)
		// Require real overlap, not a shared substring like "report".
		//
		// Compare the STEM (name minus extension), and require the match to
		// survive the job-name version bump that causes this bug in the first
		// place: a brief for "mealime_extracats" naming
		// "mealime_extracats_final_report.md" against a job's
		// "mealime_extracats3_final_report.md". Whole-filename containment
		// FAILS there — the "3" is not in the other name — so match on a
		// prefix that ignores a trailing digit run, which is exactly the shape
		// of the mistake.
		if !stemOverlap(lower, base) {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Size() == 0 {
			continue
		}
		if !notBefore.IsZero() && fi.ModTime().Before(notBefore) {
			continue
		}
		cands = append(cands, cand{path: filepath.Join(dir, name), mod: fi.ModTime()})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	return cands[0].path
}
