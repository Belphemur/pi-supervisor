package job

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFindReportNearbyFindsMisnamedReport is the regression test for a bug found
// live on mealime-extracats3: the brief told pi to write
// /tmp/mealime_extracats_final_report.md while the job's final_report was
// /tmp/mealime_extracats3_final_report.md. The agent did exactly as told, the
// gate checked the other path, and the job burned 13 rounds re-running finished
// work with last_diag "round cap reached without marker" — a diagnostic that
// blamed the marker, which had in fact been seen.
func TestFindReportNearbyFindsMisnamedReport(t *testing.T) {
	dir := t.TempDir()
	configured := filepath.Join(dir, "mealime_extracats3_final_report.md")

	// The agent wrote what the brief asked for: no "3".
	actual := filepath.Join(dir, "mealime_extracats_final_report.md")
	if err := os.WriteFile(actual, []byte("report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make sure it is newer than the notBefore floor we pass.
	now := time.Now()
	if err := os.Chtimes(actual, now, now); err != nil {
		t.Fatal(err)
	}

	got := FindReportNearby(configured, now.Add(-time.Hour))
	if got != actual {
		t.Fatalf("FindReportNearby = %q, want %q", got, actual)
	}
}

// TestFindReportNearbyIgnoresStale: a report from a PREVIOUS run must never be
// proposed as this run's, or the operator copies the wrong file.
func TestFindReportNearbyIgnoresStale(t *testing.T) {
	dir := t.TempDir()
	configured := filepath.Join(dir, "task_final_report.md")
	old := filepath.Join(dir, "task_report.md")
	if err := os.WriteFile(old, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if got := FindReportNearby(configured, time.Now()); got != "" {
		t.Fatalf("proposed a stale report from a previous run: %q", got)
	}
}

// TestFindReportNearbyIgnoresEmptyAndUnrelated keeps the lookup from guessing.
func TestFindReportNearbyIgnoresEmptyAndUnrelated(t *testing.T) {
	dir := t.TempDir()
	configured := filepath.Join(dir, "alpha_final_report.md")
	now := time.Now()

	// Empty file with a matching name: no content, so no report.
	empty := filepath.Join(dir, "alpha_final_report.round1.md")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Same directory, unrelated name: shares only the word "report".
	other := filepath.Join(dir, "unrelated-notes.md")
	if err := os.WriteFile(other, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(empty, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, now, now); err != nil {
		t.Fatal(err)
	}

	if got := FindReportNearby(configured, now.Add(-time.Hour)); got != "" {
		t.Fatalf("guessed an unrelated file: %q", got)
	}
}

// TestFindReportNearbyNewestWins: with several plausible candidates, the one
// from this run is the answer.
func TestFindReportNearbyNewestWins(t *testing.T) {
	dir := t.TempDir()
	configured := filepath.Join(dir, "run_final_report.md")

	// Same shape as the real bug: the only difference is the version digit.
	older := filepath.Join(dir, "run_final_report.md")
	newer := filepath.Join(dir, "run_final_report2.md")
	for _, p := range []string{older, newer} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t1 := time.Now().Add(-2 * time.Hour)
	t2 := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(older, t1, t1); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, t2, t2); err != nil {
		t.Fatal(err)
	}

	if got := FindReportNearby(configured, time.Now().Add(-3*time.Hour)); got != newer {
		t.Fatalf("newest should win: got %q, want %q", got, newer)
	}
}

// TestFindReportNearbyNoConfig guards the empty-path call.
func TestFindReportNearbyNoConfig(t *testing.T) {
	if got := FindReportNearby("", time.Now()); got != "" {
		t.Fatalf("no configured path must yield nothing, got %q", got)
	}
}
