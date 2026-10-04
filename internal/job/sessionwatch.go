package job

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// SessionWatcher resolves a LAUNCH's transcript by watching the session
// directory for a NEW file to appear, instead of polling for one.
//
// Why this exists: pi creates its session transcript asynchronously, so a
// directory scan issued right after the spawn routinely beats the file into
// existence and returns some older run's transcript (see FindSession). Polling
// fixed the correctness of the pick but not the latency — the supervisor could
// sit blind for a poll interval after the file landed. fsnotify removes that
// gap: the file is adopted the moment the kernel reports it, so the completion
// marker (ADR-0011) and the empty-turn detector (ADR-0010) both get a live
// surface to read on the very round that launched the session.
//
// Correctness rules, all of which the polling version had to earn the hard way:
//
//   - notBefore is a floor: a file that existed before the launch cannot be
//     this round's session, so it is never adopted (the stale-adoption bug).
//   - Only *.jsonl regular files directly in the dir are candidates.
//     _archived-stale/ is a SUBDIRECTORY and is skipped, so a quarantined
//     transcript can never be re-adopted (ADR-0010).
//   - The watch is on the DIRECTORY, not the file, because pi does not exist
//     at watch time — there is nothing to watch yet. inotify reports the
//     directory entry the instant it is created.
//
// A SessionWatcher is safe for concurrent use and must be Closed.
type SessionWatcher struct {
	dir       string
	notBefore time.Time

	mu      sync.Mutex
	watcher *fsnotify.Watcher
	closed  bool
	// found caches the adopted path so several waiters share one answer.
	found string
}

// NewSessionWatcher starts watching dir for a session transcript created at or
// after notBefore. It does not block; call Wait or TryPath.
//
// If the directory does not exist yet it is created — pi creates the munged
// sessions dir lazily, and a missing dir is the common case on round 1.
func NewSessionWatcher(worktree string, notBefore time.Time) (*SessionWatcher, error) {
	dir := MungedSessionsDir(worktree)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}
	return &SessionWatcher{dir: dir, notBefore: notBefore, watcher: w}, nil
}

// Close stops watching. It is idempotent.
func (s *SessionWatcher) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.found != "" {
		return s.watcher.Close()
	}
	return s.watcher.Close()
}

// TryPath returns the adopted transcript if one has landed, else "". It never
// blocks, so callers on a hot path can poll it cheaply.
func (s *SessionWatcher) TryPath() string {
	s.mu.Lock()
	if s.found != "" {
		defer s.mu.Unlock()
		return s.found
	}
	s.mu.Unlock()

	p := s.scan()
	if p == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// First writer wins; two waiters may both see the same file.
	if s.found == "" {
		s.found = p
	}
	return s.found
}

// Wait blocks until a transcript lands or timeout elapses, returning the path
// or "" on timeout. A non-positive timeout waits indefinitely.
//
// It still re-checks the directory on every wake: fsnotify can coalesce or drop
// events under load, and a missed event must degrade to the slower scan, never
// to a wrong answer. The scan honors the same notBefore floor, so a re-check
// cannot adopt a stale file either.
func (s *SessionWatcher) Wait(timeout time.Duration) string {
	if p := s.TryPath(); p != "" {
		return p
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	for {
		select {
		case ev, ok := <-s.watcher.Events:
			if !ok {
				// Watcher closed underneath us; fall back to a final scan.
				return s.TryPath()
			}
			if !s.eligible(ev.Name) {
				continue
			}
			if p := s.TryPath(); p != "" {
				return p
			}
		case _, ok := <-s.watcher.Errors:
			if !ok {
				return s.TryPath()
			}
			// A watch error (e.g. the dir was removed) must not lose the
			// round: fall through to the next scan on the next tick.
		case <-deadline:
			return s.TryPath()
		}
	}
}

// eligible filters a raw event path down to a candidate transcript.
func (s *SessionWatcher) eligible(name string) bool {
	if !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	// Direct children only. Quarantine's _archived-stale/ is a subdirectory,
	// so its files never reach here (ADR-0010).
	if filepath.Dir(name) != s.dir {
		return false
	}
	fi, err := os.Stat(name)
	if err != nil || fi.IsDir() {
		return false
	}
	// Same floor as the polling path: a file older than the launch belongs to
	// an earlier run, not this round.
	if !s.notBefore.IsZero() && fi.ModTime().Before(s.notBefore.Add(-sessionScanSlack)) {
		return false
	}
	return true
}

// scan is the polling fallback. It defers to dirScan — the SAME selection rule
// FindSession uses — so a watched adoption and a polled adoption can never
// disagree about which file is newest.
func (s *SessionWatcher) scan() string {
	return dirScan(s.dir, s.notBefore)
}

// dirScan picks the newest eligible file in dir. It is the single selection
// rule; FindSession and SessionWatcher both defer to it so a watched adoption
// and a polled adoption are always the same answer.
func dirScan(dir string, notBefore time.Time) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if !notBefore.IsZero() && fi.ModTime().Before(notBefore.Add(-sessionScanSlack)) {
			continue
		}
		if best == "" || fi.ModTime().After(bestMod) {
			best = filepath.Join(dir, e.Name())
			bestMod = fi.ModTime()
		}
	}
	return best
}
