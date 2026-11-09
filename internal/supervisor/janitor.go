package supervisor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pi-supervisor/internal/events"
	"pi-supervisor/internal/job"
	"pi-supervisor/internal/journal"
)

// Janitor config (ADR-0025). The values are read in runDaemon (viper via
// SetJanitorConfig's caller) into these fields; the goroutine itself never
// touches viper. Zero-value fields keep the package's defaults: disabled
// goroutines exit after nothing, and the guards in Janitor/janitorSweep
// substitute the ADR defaults for an unset interval.
const (
	janitorIntervalDefault     = time.Hour
	janitorArchiveAfterDefault = 168 * time.Hour
	// Floor on the sweep cadence (ADR-0025 §1). cast.ToDuration maps a
	// unitless numeric env value (PI_SUPERVISOR_JANITOR_INTERVAL=2) to 2ns,
	// and a ticker that fast turns the sweep into a hot loop — the old code
	// only guarded <= 0, which substitutes the default but leaves a tiny
	// positive untouched. A sweep is a directory scan plus age math ending
	// in a handful of moves; sub-minute cadence buys nothing.
	janitorIntervalFloor = time.Minute
)

// SetJanitorConfig wires the daemon-level janitor knobs (ADR-0025). Called
// from runDaemon with the values viper resolved — the goroutine never reads
// viper itself, the same pattern as the review-duration fields.
func (s *Supervisor) SetJanitorConfig(enabled bool, interval, archiveAfter time.Duration) {
	s.mu.Lock()
	s.janitorEnabled = enabled
	s.janitorInterval = interval
	s.janitorArchiveAfter = archiveAfter
	s.mu.Unlock()
}

// janitorInterval resolves the configured sweep cadence: <= 0 keeps the
// registered default, anything below the floor is clamped up to it (a
// unitless env value arrives as nanoseconds — see janitorIntervalFloor).
func janitorInterval(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return janitorIntervalDefault
	case d < janitorIntervalFloor:
		return janitorIntervalFloor
	default:
		return d
	}
}

// Janitor is the archival goroutine (ADR-0025): sweeping immediately after
// jobs load, then once per interval. janitor_enabled=false means the
// goroutine exits after nothing — the daemon-level off switch, not a
// per-job one. stop is the daemon's shared stop channel.
func (s *Supervisor) Janitor(stop chan struct{}) {
	s.mu.Lock()
	enabled := s.janitorEnabled
	interval := janitorInterval(s.janitorInterval)
	s.mu.Unlock()
	if !enabled {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	s.janitorSweep()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.janitorSweep()
		}
	}
}

// janitorEligible reports whether a persisted state is one from which no
// build round can ever resume (ADR-0025 §2). The review-terminal VERDICTS
// live on events, but a future path persisting them directly would stay
// archiveable, so the set keeps them alongside done and fatal. stopped is
// deliberately absent: a stopped job is the substrate of watch-driven resume
// (ADR-0017), and an operator stop means "stay stopped", not "delete me".
func janitorEligible(state string) bool {
	switch state {
	case "done", "fatal", "review_done", "review_exhausted":
		return true
	}
	return false
}

// endedAnchor resolves the candidate's age anchor (ADR-0025 §4.1): the
// persisted State.EndedAt when present, else the state file's mtime — for a
// terminal job that is the moment of its last terminal write. An anchor
// that cannot be determined (no valid EndedAt, no state file) reports
// false and the candidate stays for the next sweep. The stat runs WITHOUT
// any supervisor lock (the caller resolved the ended stamp under the locks
// and released them first): status/start/stop must not stall for the whole
// sweep over a large jobs directory.
func endedAnchor(name, ended string) (time.Time, bool) {
	if ended != "" {
		if t, err := time.Parse(time.RFC3339, ended); err == nil {
			return t, true
		}
		// A present-but-unparseable stamp falls through to the mtime, the
		// same fallback an empty field gets.
	}
	fi, err := os.Stat(filepath.Join(job.StateDir(), name+".json"))
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// janitorCandidate is one sweep finding: a runner that is terminal, at
// rest, un-armed, and un-tracked. Keyed by name because the def file on
// disk is what actually moves; state is carried so the per-candidate
// archive can re-validate it; ended is the raw stamp, resolved to an anchor
// AFTER the locks are released (the stat must not run under them).
type janitorCandidate struct {
	name  string
	state string
	ended string
}

// janitorSweep runs one eligibility pass and archives what it found. The
// pass under the locks is purely in-memory: eligibility (terminal state,
// not active, not armed, no open review baseline), the invalid-name guard,
// and the ended stamp captured as a string. Anchors (the per-job os.Stat)
// and the archival moves happen per candidate afterwards, lock-free;
// per-candidate errors are logged and skip the candidate, never abort the
// sweep.
func (s *Supervisor) janitorSweep() {
	now := time.Now()
	s.mu.Lock()
	after := s.janitorArchiveAfter
	if after <= 0 {
		after = janitorArchiveAfterDefault
	}
	var candidates []janitorCandidate
	for name, r := range s.jobs {
		if !job.ValidJobName(name) {
			// Never interpolate a broken name into any path (qodo #4): a def
			// with one cannot load (job.Validate refuses it), so reaching
			// here means a hand-built runner — say so and skip.
			journal.Subsys("janitor").Warn("janitor_bad_job_name", "job", name)
			continue
		}
		r.mu.Lock()
		state, active, armed := r.state.State, r.active, r.review != nil
		// A terminal job that still tracks a review baseline keeps checking
		// new threads on its PR (explicit rechecks and push re-checks read
		// the baselines from state): archiving would sever that — skip it
		// (qodo #6). The baseline is evidence the job's aftermath still has
		// an audience.
		tracked := r.state.ReviewBaseline != nil
		ended := r.state.EndedAt
		r.mu.Unlock()
		if !janitorEligible(state) || active || armed || tracked {
			continue
		}
		candidates = append(candidates, janitorCandidate{name: name, state: state, ended: ended})
	}
	s.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].name != candidates[j].name {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].state < candidates[j].state
	})
	for _, c := range candidates {
		// Anchor resolution outside the locks (kody): a terminal job's state
		// file mtime is stable unless a round runs, and losing a race here
		// only costs one sweep — the archive itself re-validates everything
		// that matters under the locks.
		anchor, anchored := endedAnchor(c.name, c.ended)
		if !anchored || now.Sub(anchor) <= after {
			continue
		}
		if err := s.janitorArchive(c); err != nil {
			// Per-candidate failure: log it, skip the candidate, keep
			// the sweep going. The def may be half-moved (ADR-0025 §5);
			// the next sweep's candidate list simply no longer contains
			// a def that is already out of jobs/.
			s.logf(c.name, "janitor: archive failed: %v", err)
			journal.Subsys("janitor").Warn("janitor_archive_failed",
				"job", c.name, "reason", err.Error())
		}
	}
}

// janitorArchive performs the move-only archival for ONE candidate
// (ADR-0025 §5), holding s.mu -> r.mu across the WHOLE critical section.
// The snapshot pass cannot make the decision safe — a Start, Restart, or
// review re-entry could activate the job in the gap between the snapshot
// and the move (qodo #1; kody: a revived fatal job then disowned by the map
// sync) — so the candidate is re-validated again here, in the same critical
// section that performs the moves and drops the runner: a concurrent Start
// either landed before the lock was taken (the revalidation skip fires) or
// waits and then finds an unknown job.
func (s *Supervisor) janitorArchive(c janitorCandidate) error {
	s.mu.Lock()
	r, ok := s.jobs[c.name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("runner vanished mid-sweep — skipped")
	}
	r.mu.Lock()
	if r.active || r.review != nil || r.state.ReviewBaseline != nil || r.state.State != c.state {
		why := fmt.Sprintf("active=%v armed=%v tracked=%v state=%s (snapshot %s)",
			r.active, r.review != nil, r.state.ReviewBaseline != nil,
			r.state.State, c.state)
		r.mu.Unlock()
		s.mu.Unlock()
		return fmt.Errorf("changed under the sweep (%s) — kept for a later one", why)
	}
	sessPath := r.job.SessionPath

	// The session is quarantined FIRST (qodo #2): a quarantine failure must
	// strand nothing — the def is still in jobs/ and the next sweep
	// re-candidates the job identically. The lock cost here and below is a
	// handful of filesystem syscalls (link/remove/rename); Stop's own 3s
	// kill wait runs after ITS unlock, so no path stalls behind us.
	qPath, err := job.Quarantine(sessPath)
	if err != nil {
		r.mu.Unlock()
		s.mu.Unlock()
		return fmt.Errorf("quarantine session: %w", err)
	}
	defPath, err := archiveDef(c.name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.mu.Unlock()
			s.mu.Unlock()
			return fmt.Errorf("move def: %w", err)
		}
		// The def already left jobs/: an earlier sweep moved it and then
		// tripped (state move, event) — finishing here cleans the runner up
		// instead of warning on every sweep forever. The archived def's
		// collision-suffixed location is unknowable from here, so the event
		// says so instead of naming a wrong path.
		defPath = ""
	}
	if _, err := archiveStateFile(c.name); err != nil {
		r.mu.Unlock()
		s.mu.Unlock()
		return fmt.Errorf("archive state file: %w", err)
	}
	// Success: targeted runner removal in the SAME critical section — the
	// janitor never hand-reloads the whole map (kody/qodo #9: the old
	// post-batch LoadJobs() dropped live runners whose defs transiently
	// failed to parse or validate; only the successfully archived runner is
	// deleted here).
	delete(s.jobs, c.name)
	wt, pr := r.job.Worktree, r.state.PRURL
	r.mu.Unlock()
	s.mu.Unlock()

	sess := qPath
	if sess == "" {
		sess = "none"
	}
	def := defPath
	if def == "" {
		def = "already-moved"
	}
	s.emitPrepared(c.name, &events.Event{Event: "job_archived"}, wt, sessPath, pr,
		fmt.Sprintf("state=%s def=%s session=%s", c.state, def, sess))
	return nil
}

// archiveDef moves jobs/<name>.json -> jobs/_archive/<name>.json (MkdirAll
// 0o755). The move is no-replace (qodo #8): see archiveMove. A name
// collision in _archive/ (a job name re-used after an earlier archive)
// advances the collision suffix — never an overwrite. Move-only: bytes
// preserved.
func archiveDef(name string) (string, error) {
	archiveDir := filepath.Join(job.JobsDir(), "_archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return "", fmt.Errorf("archive mkdir %s: %w", archiveDir, err)
	}
	return archiveMove(filepath.Join(job.JobsDir(), name+".json"), archiveDir, name+".json")
}

// archiveStateFile moves the archived job's OWN persisted state with it:
// state/<name>.json -> jobs/_archive/<name>.state.json, same collision
// scheme (qodo #3; kody: a surviving state/<name>.json made a re-used job
// name load the old terminal state — done plus the old EndedAt — and get
// archived before it could run). This is the archived job's own file
// ADR-0025 moves, not the non-goal's orphaned-state sweep: orphans whose
// defs are long gone stay untouched. A missing state file is a no-op.
func archiveStateFile(name string) (string, error) {
	src := filepath.Join(job.StateDir(), name+".json")
	if !job.Exists(src) {
		return "", nil
	}
	archiveDir := filepath.Join(job.JobsDir(), "_archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return "", fmt.Errorf("archive mkdir %s: %w", archiveDir, err)
	}
	return archiveMove(src, archiveDir, name+".state.json")
}

// archiveMove is the ONE archival mover: link-then-remove, never an
// overwrite (qodo #8) — os.Link fails with fs.ErrExist on ANY existing
// destination, including a dangling symlink (which passes os.Stat-based
// existence checks because Stat follows links), so the collision suffix
// advances instead of the destination being replaced. Same-directory
// hardlinks preserve bytes and mtime; the Remove after a successful Link is
// the only step that could drop the source bytes, so it is checked: a
// Remove failure leaves BOTH files and is reported (harmless: the candidate
// remains and the next sweep retries the suffix; cleanup is manual, and no
// byte is ever lost silently).
func archiveMove(src, destDir, base string) (string, error) {
	root := strings.TrimSuffix(base, filepath.Ext(base))
	ext := filepath.Ext(base)
	ts := time.Now().Format("2006-01-02T15-04-05")
	for i := 0; ; i++ {
		var name string
		switch i {
		case 0:
			name = base
		case 1:
			name = root + "_" + ts + ext
		default:
			name = fmt.Sprintf("%s_%s_%d%s", root, ts, i-1, ext)
		}
		dest := filepath.Join(destDir, name)
		err := os.Link(src, dest)
		if err == nil {
			if rmErr := os.Remove(src); rmErr != nil {
				return "", fmt.Errorf("move %s -> %s: linked, but source remove failed: %w",
					src, dest, rmErr)
			}
			return dest, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("move %s -> %s: %w", src, dest, err)
		}
	}
}
