package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

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

// Janitor is the archival goroutine (ADR-0025): sweeping immediately after
// jobs load, then once per interval. janitor_enabled=false means the
// goroutine exits after nothing — the daemon-level off switch, not a
// per-job one. stop is the daemon's shared stop channel.
func (s *Supervisor) Janitor(stop chan struct{}) {
	s.mu.Lock()
	enabled, interval := s.janitorEnabled, s.janitorInterval
	s.mu.Unlock()
	if !enabled {
		return
	}
	if interval <= 0 {
		interval = janitorIntervalDefault
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
// false and the candidate stays for the next sweep.
func endedAnchor(name string, st job.State) (time.Time, bool) {
	if st.EndedAt != "" {
		if t, err := time.Parse(time.RFC3339, st.EndedAt); err == nil {
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
// rest, un-armed, and old enough — keyed by name because the def file on
// disk is what actually moves.
type janitorCandidate struct {
	name  string
	state string
}

// janitorSweep runs one eligibility pass and archives what it found. It
// snapshots candidates under s.mu (brief: no forked state checks mid-move),
// then performs the move-only actions lock-free; per-candidate errors are
// logged and skip the candidate, never abort the sweep.
func (s *Supervisor) janitorSweep() {
	now := time.Now()
	s.mu.Lock()
	after := s.janitorArchiveAfter
	if after <= 0 {
		after = janitorArchiveAfterDefault
	}
	var candidates []janitorCandidate
	for name, r := range s.jobs {
		r.mu.Lock()
		state, active, armed := r.state.State, r.active, r.review != nil
		anchor, anchored := endedAnchor(name, r.state)
		r.mu.Unlock()
		if !janitorEligible(state) || active || armed || !anchored {
			continue
		}
		if now.Sub(anchor) <= after {
			continue
		}
		candidates = append(candidates, janitorCandidate{name: name, state: state})
	}
	s.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].name != candidates[j].name {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].state < candidates[j].state
	})
	if len(candidates) > 0 {
		for _, c := range candidates {
			if err := s.janitorArchive(c); err != nil {
				// Per-candidate failure: log it, skip the candidate, keep
				// the sweep going. The def may be half-moved (ADR-0025 §5);
				// the next sweep's candidate list simply no longer contains
				// a def that is already out of jobs/.
				s.logf(c.name, "janitor: archive failed: %v", err)
				journal.Subsys("janitor").Warn("janitor_archive_failed",
					"job", c.name, "reason", err.Error())
				continue
			}
		}
		// Map sync rides the tested reload path; even after partial
		// failures, any successful move means the runner must drop, and
		// LoadJobs re-derives the map from the dir.
		if err := s.LoadJobs(); err != nil {
			journal.Subsys("janitor").Warn("janitor_reload_failed",
				"reason", err.Error())
		}
	}
}

// janitorArchive performs the move-only archival for ONE candidate:
// def → jobs/_archive/, session → _archived-stale/, then the job_archived
// event through the single emit funnel. No deletion on either layer.
func (s *Supervisor) janitorArchive(c janitorCandidate) error {
	defPath, err := archiveDef(c.name)
	if err != nil {
		return err
	}
	sessPath := ""
	s.mu.Lock()
	if r, ok := s.jobs[c.name]; ok {
		r.mu.Lock()
		sessPath = r.job.SessionPath
		r.mu.Unlock()
	}
	s.mu.Unlock()
	qPath, err := job.Quarantine(sessPath)
	if err != nil {
		return fmt.Errorf("quarantine session: %w", err)
	}
	sess := qPath
	if sess == "" {
		sess = "none"
	}
	// Emitted BEFORE the LoadJobs sync, while the runner is still in the
	// map — emitEnvelope fills worktree/session context from it (ADR-0025
	// §6: NOT terminal, nothing added to events.Terminal or the watch exit
	// list).
	s.emit(c.name, "job_archived", 0, 0, 0, "",
		"state=%s def=%s session=%s", c.state, defPath, sess)
	return nil
}

// archiveDef moves jobs/<name>.json → jobs/_archive/<name>.json (MkdirAll
// 0o755). A name collision in _archive/ (a job name re-used after an
// earlier archive) gains job.Quarantine's <name>_<2006-01-02T15-04-05>
// suffix plus a numeric counter on same-second collision — never an
// overwrite. Move-only: bytes preserved.
func archiveDef(name string) (string, error) {
	src := filepath.Join(job.JobsDir(), name+".json")
	archiveDir := filepath.Join(job.JobsDir(), "_archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return "", fmt.Errorf("archive mkdir %s: %w", archiveDir, err)
	}
	dest := filepath.Join(archiveDir, name+".json")
	if job.Exists(dest) {
		ts := time.Now().Format("2006-01-02T15-04-05")
		for i := 1; ; i++ {
			var coll string
			if i == 1 {
				coll = filepath.Join(archiveDir, name+"_"+ts+".json")
			} else {
				coll = filepath.Join(archiveDir, fmt.Sprintf("%s_%s_%d.json", name, ts, i))
			}
			if !job.Exists(coll) {
				dest = coll
				break
			}
		}
	}
	if err := os.Rename(src, dest); err != nil {
		return "", fmt.Errorf("archive rename %s -> %s: %w", src, dest, err)
	}
	return dest, nil
}
