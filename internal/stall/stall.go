// Package stall detects when a supervised pi agent has stalled waiting on
// CI / code review (the answer-code-review loop): it tails the session JSONL,
// flags markers of the CI-wait path, and reports a stall when the transcript
// then goes quiet for the configured patience window (doc/adr/0004).
package stall

import (
	"io"
	"os"
	"regexp"
	"time"
)

// ciRe matches the agent's own words when it is parked on the CI / review
// loop, scanned over the raw JSONL line (robust to schema drift). Matched
// case-insensitively; includes the answer-code-review skill name the owner's
// PR workflow uses.
var ciRe = regexp.MustCompile(`(?i)(` +
	`waiting (for|on)[^\n]{0,60}\b(ci|checks?|pipeline|review|workflow|tests?|builds?)\b` +
	`|gh pr (checks|reviews|status|view)` +
	`|answer-code-review` +
	`|re-?review\b` +
	`|checks? (are |is |still )+(running|pending|queued|in progress)` +
	`|\bci\b[^\n]{0,40}\b(pending|running|queued|failed)\b` +
	`|review loop` +
	`)`)

// prRe matches a GitHub pull-request URL in the transcript. Pinned to the
// full pull path (`/pull/<number>`): a bare truncated `pull/` fragment (a
// partially-flushed line, or the agent typing the URL in progress) must NOT
// match, otherwise status would advertise a URL that 404s. Owner/repo are
// any non-slash, non-space run; the number must be digits (ADR-0006).
var prRe = regexp.MustCompile(`https://github\.com/[^/" ]+/[^/" ]+/pull/[0-9]+`)

// Detector tails one session JSONL.
type Detector struct {
	path       string
	idle       time.Duration // quiet time after the last CI marker that constitutes a stall
	offset     int64         // next byte to read
	size       int64         // last known file size
	lastGrowth time.Time     // when bytes were last seen
	ciMode     bool          // armed by a CI/review marker in recent content
	marker     string        // most recent matched text
	prURL      string        // first PR URL seen since the detector was created
}

// New starts detection at the file's current end, so historical content (the
// round-1 brief echo, older rounds) never arms it.
func New(path string, idle time.Duration) *Detector {
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	return &Detector{path: path, offset: size, size: size, lastGrowth: time.Now(), idle: idle}
}

// Poll reads any new JSONL content and evaluates the stall condition:
// a CI/review marker was seen, and the transcript has been quiet for the
// idle window since. On a stall it disarms ciMode so a later stall (agent
// resumed, parked again) can fire independently. Missing/rotated files are
// tolerated (treated as "no new content").
//
// A transcript that SHRINKS (rotation, or a pi run that restarts the file) is
// re-based to the new end: without this the offset would stay past EOF and
// the detector would be blind for the rest of the round.
//
// Poll also records the FIRST pull-request URL it sees (PRURL); the first one
// wins because it is the one the agent opened in this round. The scrape is
// best-effort: an agent that opens a PR but never links it leaves PRURL empty
// (ADR-0006).
func (d *Detector) Poll() (stalled bool, marker string) {
	fi, err := os.Stat(d.path)
	if err != nil {
		return false, ""
	}
	if fi.Size() < d.offset {
		// Rotated/truncated: restart from the new end, disarmed.
		d.offset, d.size, d.ciMode, d.marker = fi.Size(), fi.Size(), false, ""
	}
	if fi.Size() > d.size {
		d.lastGrowth = time.Now()
	}
	d.size = fi.Size()
	if fi.Size() > d.offset {
		f, err := os.Open(d.path)
		if err == nil {
			buf := make([]byte, fi.Size()-d.offset)
			n, rerr := f.ReadAt(buf, d.offset)
			d.offset += int64(n)
			_ = f.Close()
			if rerr == nil || rerr == io.EOF {
				if d.prURL == "" {
					if m := prRe.Find(buf); m != nil {
						d.prURL = string(m)
					}
				}
				if m := ciRe.Find(buf); len(m) > 0 {
					d.ciMode = true
					d.marker = string(m)
				}
			}
		}
	}
	if d.ciMode && time.Since(d.lastGrowth) >= d.idle {
		d.ciMode = false // re-arm for the next park
		return true, d.marker
	}
	return false, ""
}

// PRURL is the first pull-request URL seen in the transcript since New, or ""
// if the agent has not linked one (ADR-0006).
func (d *Detector) PRURL() string { return d.prURL }
