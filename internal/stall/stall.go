// Package stall detects when a supervised pi agent has stalled waiting on
// CI / code review (the answer-code-review loop): it tails the session JSONL,
// flags markers of the CI-wait path, and reports a stall when the transcript
// then goes quiet for the configured patience window (doc/adr/0004).
package stall

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
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
	// Empty-turn stall bookkeeping (ADR-0010).
	lastGrowthSize int64 // file size at the last empty-turn evaluation
	toolCalls      int   // tool_use markers seen since the last empty-turn evaluation
	// toolsInFlight is tool calls issued but not yet returned. A transcript is
	// frozen while a long tool runs (`go test -race`, a CI poll, a push), and
	// that freeze is indistinguishable from a hang by growth alone. An
	// outstanding call is positive evidence of work in progress, so it must
	// suppress the empty-turn stall instead of being consumed as a one-time
	// reprieve.
	toolsInFlight int
}

// New starts detection at the file's current end, so historical content (the
// round-1 brief echo, older rounds) never arms it.
func New(path string, idle time.Duration) *Detector {
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	return &Detector{
		path: path, offset: size, size: size, lastGrowth: time.Now(),
		lastGrowthSize: size, idle: idle,
	}
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
		// Rotated/truncated: restart from the new end, disarmed. The tool
		// counters reset WITH the rest: they describe the file that was
		// being tailed, and after a rotation the detector is reading a
		// different file — carrying toolCalls/toolsInFlight across would
		// leave a call seen in flight just before the rotation pinning the
		// counter above zero forever, muting EmptyTurn for the whole round
		// (the ADR-0010 failure mode the detector exists to catch).
		d.offset, d.size, d.ciMode, d.marker = fi.Size(), fi.Size(), false, ""
		d.toolCalls, d.toolsInFlight = 0, 0
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
				// Count tool invocations in this slice so EmptyTurn can tell
				// "streaming prose" from "wedged with no tool call" (ADR-0010).
				// Issued and returned are counted separately so a call that is
				// STILL RUNNING leaves toolsInFlight > 0 — a long `go test` or CI
				// poll freezes the transcript exactly like a hang, and only the
				// outstanding count tells the two apart.
				uses := len(toolUseRe.FindAll(buf, -1))
				results := countResultLines(buf)
				d.toolCalls += uses
				d.toolsInFlight += uses - results
				if d.toolsInFlight < 0 {
					// A result can be observed for a call issued before this
					// detector started (New begins mid-file). Never let the
					// count go negative and mask a real in-flight call.
					d.toolsInFlight = 0
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

// PRURLScanner finds the last PR link in a transcript that is still growing.
//
// It exists because the two obvious implementations are each wrong in one
// direction, and BOTH shipped here first:
//
//   - tail-only: cheap, but drops a link written more than prURLTailBytes from
//     the end, so a long job that opened its PR early and then emitted megabytes
//     of CI output reported "not linked" — the exact ADR-0006 failure the scrape
//     exists to prevent;
//   - whole-file: correct, but re-read a monotonically growing file on every
//     round for a job that never links a PR — O(rounds x transcript size).
//
// So it scans INCREMENTALLY: it remembers the offset already read and only ever
// reads forward. The window bounds a SINGLE read; the offset bounds the TOTAL. A
// transcript is append-only, so bytes already scanned cannot change meaning, and
// a truncate/rotate (size below the offset) resets it.
//
// A scanner is per-transcript and safe for concurrent use; give each job its own.
// Returns "" when there is no link, which still means "not linked", never "no PR
// exists".
type PRURLScanner struct {
	mu       sync.Mutex
	path     string
	offset   int64
	lastSize int64
	lastIno  uint64
	last     string
}

// NewPRURLScanner starts a scanner for one transcript path.
func NewPRURLScanner(path string) *PRURLScanner {
	return &PRURLScanner{path: path}
}

// Path is the transcript this scanner reads, so a caller holding one per job can
// tell whether it still matches the job's current session (a restart or
// `restart --fresh` changes it).
func (s *PRURLScanner) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// prOverlapBytes is how far back each incremental read reaches before the recorded
// offset. It exists so a link STRADDLING a read boundary is never split: the
// overlap is re-scanned and the match found whole. A PR URL is far shorter.
const prOverlapBytes = 4 << 10

// PRURL returns the last PR link found so far, reading only bytes appended since
// the previous call.
func (s *PRURLScanner) PRURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Stat(s.path)
	if err != nil {
		return s.last
	}
	size := fi.Size()
	ino, inoOK := inodeOf(fi)
	if size < s.offset {
		// Truncated or rotated: the old bytes are gone and the offset is
		// meaningless, so start over rather than read from a bogus position.
		s.offset, s.last, s.lastSize, s.lastIno = 0, "", 0, ino
	}
	// Detect a REPLACEMENT, not just a shrink. A rewrite or rotation can produce a
	// different file of EXACTLY the same length, which no size comparison can see:
	// the first attempt asserted fi.Sys() against an Ino() METHOD interface, but on
	// every Unix Ino is a FIELD of *syscall.Stat_t, so the assertion matched
	// nothing and the check was dead code (found by live probe: iface=false,
	// stat_t=true) — and a size-heuristic variant was wrong in the other direction,
	// because a caught-up scanner sits at offset==size and never satisfies
	// "offset behind". The inode is the signal that works, compared BEFORE it is
	// recorded; lastIno==0 means "first observation, nothing to compare yet".
	//
	// Where the platform hides the inode, the scanner degrades to trusting size —
	// the pre-inode behavior, never a failure.
	if inoOK && s.lastIno != 0 && ino != s.lastIno {
		s.rewind()
		s.lastIno = ino
	}
	if inoOK {
		s.lastIno = ino
	}
	if size == s.lastSize && s.offset >= size {
		return s.last // nothing appended since the last call
	}

	// Read FORWARD from the offset in bounded chunks, never jumping to the tail.
	//
	// Jumping to the tail skipped the middle: when more than one chunk arrived
	// between two calls, every byte in [offset, size-chunk] was never scanned, so
	// a link written there was lost forever — the ADR-0006 failure again, for a
	// job that emits a lot in a single round. The chunk bounds a SINGLE read; the
	// offset bounds the TOTAL, so correctness never depends on one append being
	// small.
	readTo := min(s.offset+prURLChunkBytes, size)
	// Re-read a little before the offset so a link straddling a chunk boundary is
	// not split. A PR URL is far shorter than the overlap.
	from := s.offset
	if from > prOverlapBytes {
		from -= prOverlapBytes
	}

	buf := make([]byte, readTo-from)
	n, _ := readFileAt(s.path, buf, from)
	// Commit the offset to the raw end of what was read — NOT to the last
	// complete line. Line bookkeeping here was tried and both variants fail:
	// committing from+n lands mid-line, so the partial-line trim had to exist,
	// and that trim ate the unscanned tail of a line longer than the overlap
	// whenever a chunk boundary straddled it (kody PR #5 — a link past the
	// 256KiB boundary in a long JSON line was gone permanently); committing
	// only to the last newline instead makes the offset STOP advancing for a
	// single line longer than the chunk, so bytes beyond it are never scanned.
	//
	// The overlap makes both unnecessary: every read re-scans the 4KiB before
	// the raw end, and a PR URL is under 255 bytes, so a URL straddling the
	// chunk boundary is fully inside the NEXT call's window. A split match
	// (prefix in this call, digits in the next) cannot false-positive either,
	// because prRe requires the full "https://…/pull/" prefix. Scanning is
	// pure bytes; the offset is pure bytes.
	if n > 0 {
		s.offset = from + int64(n)
	}
	// lastSize is the "nothing new" short-circuit ONLY when the backlog is fully
	// drained. Setting it while bytes remain would make the NEXT call return early
	// and the rest of the backlog would never be scanned — the same silent skip in
	// a different guise.
	if s.offset >= size {
		s.lastSize = size
	} else {
		s.lastSize = -1 // force the next call to keep reading
	}
	if n <= 0 {
		return s.last
	}
	buf = buf[:n]
	// No partial-line trimming on this buffer, on purpose — see the comment above
	// the offset commit: the overlap already re-reads every byte a split match
	// could straddle, and trimming has twice cost real links.
	if all := prRe.FindAll(buf, -1); len(all) > 0 {
		s.last = string(all[len(all)-1])
	}
	return s.last
}

// prURLChunkBytes bounds ONE read. Several calls may be needed to catch up on a
// large append; each consumes at most this much and the next continues from the
// offset. Bounding a read is not the same as bounding what gets scanned.
const prURLChunkBytes = 256 << 10

// modelChunkBytes bounds ONE read of the model scanner. Same rationale as
// prURLChunkBytes: bounding a read is not the same as bounding what gets
// scanned — the offset bounds the total, and the partial-line carry makes
// sure a record split across reads is parsed whole.
const modelChunkBytes = 256 << 10

// modelMaxPartial caps the carried incomplete trailing line. An assistant
// record is a few KB; compaction records can reach megabytes. A line longer
// than the cap has its model missed for THIS record (the offset has already
// committed past it) — the honest tradeoff vs unbounded memory, and the
// same cap discipline the client's control reader uses.
const modelMaxPartial = 8 << 20

// transcriptModelRecord is the slice of a session JSONL line the model
// scanner needs. Parsed with encoding/json — never regex — so field
// reordering, escaping, and formatting changes in pi's writer cannot break
// the scrape (owner directive 2026-10-09). Only assistant records carry a
// model: `message.role == "assistant"` plus a non-empty `message.model`.
// A bare `"model"` string quoted in CONTENT (briefs, steers, toolResults)
// names a request, not an answer, and is never inspected here.
type transcriptModelRecord struct {
	// Type must be "message" (qodo PR#12 finding 6): any nested
	// assistant-SHAPED object inside a different record type (compaction
	// summaries, tool payloads) must not impersonate an answering model.
	Type    string `json:"type"`
	Message *struct {
		Role     string `json:"role"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Usage    *struct {
			TotalTokens int64 `json:"totalTokens"`
			Cost        *struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
	} `json:"message"`
}

// modelIdentity renders "provider/model" — the canonical surface form.
func modelIdentity(provider, model string) string {
	if provider == "" {
		return model
	}
	return provider + "/" + model
}

// ModelScanner is the incremental model-identity scanner for one transcript
// (ADR-0021). Same discipline as PRURLScanner: offset + inode rotation check,
// append-only, never re-reads the whole file — but it parses complete JSONL
// records (json.Unmarshal per line, with an incomplete trailing line carried
// to the next read) instead of regexing raw bytes, so pi's writer can
// reorder or reformat fields without breaking identity extraction. It is
// the ONE component that derives model identity from the transcript;
// nothing else re-derives it.
type ModelScanner struct {
	mu       sync.Mutex
	path     string
	offset   int64
	lastSize int64
	lastIno  uint64
	// head is a content fingerprint of the first headProbeBytes: mtime has
	// COARSE granularity on some filesystems (second-resolution — a
	// same-tick rewrite leaves it unchanged, kody PR#11), so the
	// rewrite detector needs a content signal. head+size+inode together
	// make a same-content false positive require an identical prefix AND
	// identical size — which for a transcript rewrite means the models
	// would be identical anyway.
	head []byte
	// lastMod/lastModSet back the beyond-probe edit detector (qodo PR#12
	// findings 3+4): a bumped mtime on a caught-up or growing file whose
	// head probe is unchanged means bytes beyond the 4KiB probe were edited
	// — reset and re-scan. lastModSet distinguishes "never seen" from
	// "seen at t0" on coarse-mtime filesystems.
	lastMod    time.Time
	lastModSet bool
	// partial carries the incomplete trailing line between reads: the offset
	// commits to the raw end of what was read, so a record straddling a
	// chunk boundary is only parsed once its newline has landed. A []byte
	// grown by append (never re-concatenated) — kody PR#12: string concat
	// copied O(N²/chunk) bytes while a multi-MB compaction record grew.
	partial []byte
	// last is the most recent model seen (current_model); order remembers
	// every distinct model in first-seen order (models_used).
	last    string
	seen    []string
	seenSet map[string]bool
	// lastTokens / lastCost mirror the LATEST assistant record's cumulative
	// usage (ADR-0021 extension, owner directive 2026-10-10): pi's
	// usage.totalTokens and usage.cost.total are ALREADY running totals per
	// record, so the scanner keeps the latest — never sums (summing would
	// double-count every cached turn). Zero until the first record with
	// usage lands.
	lastTokens int64
	lastCost   float64
}

// headProbeBytes is how much of the file head the rewrite detector
// fingerprints. An assistant record is a few KB, so 4KiB always covers the
// first record's identity.
const headProbeBytes = 4 << 10

// readHead fingerprints the file head; a short/unreadable file returns what
// it can (nil = unknown, treated as "same" so a transient stat error never
// resets a healthy scanner).
func readHead(path string) []byte {
	buf := make([]byte, headProbeBytes)
	n, err := readFileAt(path, buf, 0)
	// ReadAt returns io.EOF when the file is shorter than the buffer — that
	// is a SUCCESS here (the head is the whole file). Same trap the PR
	// scanner's readers document.
	if n <= 0 {
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return buf[:n]
}

// NewModelScanner starts a scanner for one transcript path.
func NewModelScanner(path string) *ModelScanner {
	return &ModelScanner{path: path, seenSet: map[string]bool{}}
}

// Path is the transcript this scanner reads, so a caller holding one per job
// can tell whether it still matches the job's current session.
func (s *ModelScanner) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// Current is the most recent model seen ("provider/model"), "" before the
// first assistant record lands.
func (s *ModelScanner) Current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// LastUsage is the LATEST assistant record's cumulative usage (ADR-0021
// extension): pi's usage.totalTokens / usage.cost.total are already running
// totals, so the scanner keeps the latest rather than summing. Zero values
// mean no record with usage has landed yet.
func (s *ModelScanner) LastUsage() (totalTokens int64, totalCost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTokens, s.lastCost
}

// Used is every distinct model in first-seen order. The slice is copied so
// the caller cannot mutate the scanner's view.
func (s *ModelScanner) Used() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copySeen()
}

// Scan reads bytes appended since the previous call, parses every COMPLETE
// line as JSON, and updates the current model and the used list. "" returns
// are normal: no assistant record has landed yet, or nothing new was
// appended.
func (s *ModelScanner) Scan() (current string, used []string, consumed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Stat(s.path)
	if err != nil {
		return s.last, s.copySeen(), false
	}
	size := fi.Size()
	ino, inoOK := inodeOf(fi)
	// Content fingerprint (kody PR#11: coarse mtimes make ModTime alone
	// unreliable — a same-tick rewrite leaves it unchanged). head+size is
	// the rewrite signal; inode is kept for the wholesale-replacement case.
	head := readHead(s.path)
	sameHead := len(head) == 0 || string(head) == string(s.head)

	// Truncated/rotated/replaced — same discipline as PRURLScanner: the
	// offset is meaningless, start over. A replacement file is NEW content:
	// the used list resets with everything else, because keeping `seen`
	// while clearing `last` would surface a current model missing from the
	// used list. (A restart --fresh changes the path and recreates the
	// scanner wholesale; this path covers in-place rewrites.)
	if size < s.offset {
		s.reset()
	}
	if inoOK && s.lastIno != 0 && ino != s.lastIno {
		s.reset()
		sameHead = true // the inode reset already cleared state; don't double-reset
	}
	if inoOK {
		s.lastIno = ino
	}
	// In-place rewrite detection (kody PR#11): a remove+recreate can reuse
	// the inode AND the size, so the content fingerprint is the signal. Two
	// windows must both be covered:
	//   - caught up (offset>=size): the classic case — without the reset the
	//     nothing-new short-circuit reports the stale model forever.
	//   - MID-CATCH-UP (lastSize==-1 sentinel): the old offset points into
	//     the NEW file's middle; the head probe already differs, so reset and
	//     re-read the replacement from byte 0 instead of skipping its head.
	if s.head != nil && !sameHead && size == s.lastSize && s.offset >= size {
		s.reset()
	} else if s.head != nil && !sameHead && s.lastSize == -1 {
		s.reset()
	}
	// qodo PR#12 findings 3+4, CORRECTED per kody's re-review: the mtime
	// reset fires ONLY when the scanner is CAUGHT UP (offset >= size). The
	// earlier "size > lastSize" form re-read the ENTIRE transcript on every
	// 5s tick during an active round — every append bumps mtime, so the
	// cost grew with transcript size × tick count for nothing. A caught-up
	// file with a bumped mtime is an unambiguous in-place rewrite (nothing
	// new to read, yet the content changed). The prefix-preserving GROWING
	// rewrite is deliberately not reset: it is indistinguishable from an
	// append without re-reading, the delta read still parses the new tail
	// records, and the head probe plus this caught-up path cover the other
	// shapes. Documented limitation, not a silent wrong-answer path.
	if s.lastModSet && !fi.ModTime().Equal(s.lastMod) &&
		s.offset >= size && s.lastSize > 0 {
		s.reset()
	}
	if len(head) > 0 {
		s.head = head
	}
	s.lastMod = fi.ModTime()
	s.lastModSet = true
	// Nothing-new short-circuit: content fingerprint, size, and offset all
	// agree (mtime is deliberately NOT part of this — coarse-granularity
	// mtimes made the old check flaky).
	if size == s.lastSize && s.offset >= size && sameHead {
		return s.last, s.copySeen(), false
	}

	readTo := min(s.offset+modelChunkBytes, size)
	buf := make([]byte, readTo-s.offset)
	n, _ := readFileAt(s.path, buf, s.offset)
	if n > 0 {
		s.offset += int64(n)
	}
	if s.offset >= size {
		s.lastSize = size
	} else {
		s.lastSize = -1 // force the next call to keep reading
	}
	if n <= 0 {
		return s.last, s.copySeen(), false
	}

	// Append the new chunk to the carried partial WITHOUT re-copying the
	// whole thing per chunk (kody PR#12: string concat was O(N²/chunk) on a
	// growing multi-MB line — ~130MiB copied for an 8MiB record). Search
	// only the NEW region for the last newline; the carried prefix is
	// already newline-free by construction.
	s.partial = append(s.partial, buf[:n]...)
	newRegion := len(s.partial) - n
	if i := bytes.LastIndexByte(s.partial[newRegion:], '\n'); i >= 0 {
		abs := newRegion + i
		whole := s.partial[:abs+1]
		rest := append(s.partial[:0], s.partial[abs+1:]...)
		s.partial = rest
		if len(s.partial) > modelMaxPartial {
			// Carried line exceeds the cap: drop it (model missed, offset
			// already committed past it — later records are unaffected).
			s.partial = s.partial[:0]
		}
		s.scanLines(string(whole))
		return s.last, s.copySeen(), true
	}
	// No newline in this read: the whole chunk is one growing line. The
	// offset advanced, so consumed MUST be true — the drain loops break on
	// false and would otherwise stop mid-record (kody PR#11 re-review).
	if len(s.partial) > modelMaxPartial {
		// Runaway line (a multi-MB record): drop it rather than grow
		// without bound. Its model is missed; the offset has already
		// committed past it, so later records are unaffected.
		s.partial = s.partial[:0]
		return s.last, s.copySeen(), true
	}
	return s.last, s.copySeen(), true
}

// scanLines parses whole LF-terminated lines and folds assistant records'
// model identities into last/seen.
func (s *ModelScanner) scanLines(data string) {
	for line := range strings.SplitSeq(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec transcriptModelRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue // non-JSON noise: ignore, matching the client reader
		}
		if rec.Type != "" && rec.Type != "message" {
			continue // qodo PR#12 finding 6: only real message records answer
		}
		if rec.Message == nil || rec.Message.Role != "assistant" || rec.Message.Model == "" {
			continue
		}
		s.last = modelIdentity(rec.Message.Provider, rec.Message.Model)
		if !s.seenSet[s.last] {
			s.seenSet[s.last] = true
			s.seen = append(s.seen, s.last)
		}
		// Usage: keep the LATEST record's totals verbatim (owner directive
		// 2026-10-10). Zero and positive are adopted ALIKE (qodo PR#12
		// finding 5): a failover record that reports 0 cost is the newest
		// truth — keeping the previous paid figure would publish a cost the
		// current model never incurred. pi reports these as running totals
		// per record; summing would double-count every cached turn.
		if u := rec.Message.Usage; u != nil {
			s.lastTokens = u.TotalTokens
			if u.Cost != nil {
				s.lastCost = u.Cost.Total
			}
		}
	}
}

func (s *ModelScanner) reset() {
	s.offset, s.lastSize = 0, 0
	s.partial = s.partial[:0]
	s.last = ""
	s.seen, s.seenSet = nil, map[string]bool{}
	s.lastTokens, s.lastCost = 0, 0
	s.lastMod, s.lastModSet = time.Time{}, false
}

func (s *ModelScanner) copySeen() []string {
	out := make([]string, len(s.seen))
	copy(out, s.seen)
	return out
}

// readFileAt reads into buf at off, tolerating a short read: a PR link near the
// tail is exactly what we came for, so partial data is better than none.
func readFileAt(path string, buf []byte, off int64) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return f.ReadAt(buf, off)
}

// rewind resets the scanner's position and cached link, so the next call
// re-scans from byte 0. Shared by the truncate path and the replacement path,
// so there is one place that decides what "start over" means. lastIno is left
// alone: the caller has already established the new file's identity.
func (s *PRURLScanner) rewind() {
	s.offset, s.last, s.lastSize = 0, "", 0
}

// EmptyTurnWindow is the quiet window that turns "alive but producing nothing"
// into an empty-turn stall (ADR-0010): the transcript grew, but the agent
// emitted no assistant text and no tool_use for the whole window, which the
// lowpower-stats campaign showed can burn a full timeout_s with zero progress
// and no error signal.
type EmptyTurnWindow struct {
	// Idle is how long the transcript may stay under MinGrowth bytes without a
	// tool call before the round is classified as an empty-turn stall.
	Idle time.Duration
	// MinGrowth is the byte threshold that still counts as "no progress".
	MinGrowth int64
}

// toolUseRe matches a tool invocation in a transcript line. Counted over the
// raw JSONL so it survives schema drift: any "tool_use" type marker counts.
//
// This counts a call being REQUESTED, not a call returning. The two are
// tracked separately (toolResultRe) so a call that is still in flight can be
// told apart from one that has already come back.
var toolUseRe = regexp.MustCompile(`"tool_use"`)

// toolResultRe matches the RETURN of a tool call. pi writes each result as
// its own JSONL line — `"role":"toolResult"` (with a toolCallId) — and the
// `"tool_use_id"` spelling is the Anthropic-shaped equivalent, so the pairing
// survives either schema. Both markers live on the SAME result line, which is
// why matching must be per LINE (countResultLines), never a raw substring
// count over the buffer: the old alternation ("toolResult"|"toolCallId"|
// "tool_use_id") hit a real result line TWICE and drained toolsInFlight at
// double rate, re-arming the very abort-a-live-tool bug the counter exists to
// prevent (commit 18d48d4).
var toolResultRe = regexp.MustCompile(`"toolResult"|"tool_use_id"`)

// countResultLines counts tool-result LINES, not substring occurrences: one
// JSONL line is one tool result, however many of its fields mention it.
func countResultLines(buf []byte) int {
	n := 0
	for line := range bytes.SplitSeq(buf, []byte{'\n'}) {
		if len(line) != 0 && toolResultRe.Match(line) {
			n++
		}
	}
	return n
}

// EmptyTurn reports whether the round is stalled on an empty turn: the
// transcript has not grown by MinGrowth bytes for Idle, AND no tool_use has
// been seen in that same window. Growth without a tool call is normal work
// (the model is streaming text), so both conditions must hold — that pairing
// is what separates "thinking" from "wedged".
func (d *Detector) EmptyTurn(w EmptyTurnWindow) (stalled bool, quietFor time.Duration) {
	if w.Idle <= 0 {
		w.Idle = 60 * time.Second
	}
	if w.MinGrowth <= 0 {
		w.MinGrowth = 1
	}
	// Re-stat rather than trusting the last Poll: this is an independent
	// question ("is the round producing anything at all?") and must not
	// silently answer from a stale snapshot.
	size := d.size
	if fi, err := os.Stat(d.path); err == nil {
		size = fi.Size()
		if size > d.size {
			d.lastGrowth = time.Now()
			d.size = size
		}
	}
	quiet := time.Since(d.lastGrowth)
	if quiet < w.Idle {
		return false, quiet
	}
	// A tool call that was issued and has not returned is ACTIVE WORK, not a
	// stall. The transcript is frozen for the whole duration of a long tool
	// (`go test -race` on a loaded box, a CI poll, a git push), so growth alone
	// cannot tell a running tool from a dead agent — but an outstanding call
	// can.
	//
	// This check must come BEFORE the toolCalls reset below. The old code
	// treated "a tool call was seen" as a one-time reprieve and then zeroed the
	// counter, so a tool still running at the next tick got its turn aborted
	// mid-execution. That is not hypothetical: it killed the enum job's round 5
	// while the agent was pushing its branch, and the aborted SIGINT surfaced
	// as exit 130 with a healthy transcript.
	if d.toolsInFlight > 0 {
		return false, quiet
	}
	// Growth inside the window: the agent IS producing, whatever it is. Only a
	// transcript frozen below the threshold can be an empty turn.
	if size-d.lastGrowthSize >= w.MinGrowth {
		d.lastGrowthSize = size
		return false, quiet
	}
	if d.toolCalls > 0 {
		d.toolCalls = 0
		d.lastGrowthSize = size
		return false, quiet
	}
	return true, quiet
}
