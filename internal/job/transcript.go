package job

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
)

// Completion detection reads the SESSION TRANSCRIPT, not the run log
// (ADR-0011). round() truncates /tmp/pi_<job>_run.log at the start of every
// round, so asking that per-round, destroyed-every-round artifact a
// cumulative question ("did this job ever finish?") livelocks a job that
// already emitted its marker. The transcript is append-only and survives
// every round boundary.
//
// Ground truth from the real failing transcript (mealime-roomux, 1.7MB, 14
// rounds): the persisted session log has NO text_delta / assistantMessageEvent
// records. Its shape is:
//
//	type: "message"        message.role: user | assistant | toolResult
//	type: "custom"         customType: "dcp-state"  (DCP compression summaries)
//	type: "session" / "session_info" / "model_change" / ...
//
// So the marker must be read from assistant TEXT BLOCKS. Scanning whole
// records would be wrong: DCP `custom` summaries and `bash` toolCall arguments
// both QUOTE the marker (27 + 1 false hits in the real file), some of them
// before the work was done. Restricting to role=assistant text blocks is what
// makes this a completion signal rather than a string match.

// TranscriptWindow is how many trailing bytes of the transcript a completion
// scan reads. It matches TailLines' window and is a PERFORMANCE bound only: a
// marker inside the window is found regardless of where in the window it sits.
const TranscriptWindow = 256 << 10

// transcriptRecord is the subset of a persisted session record we read.
// `message` is kept raw because its shape varies by role.
type transcriptRecord struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

// persistedMessage is the role/content envelope of a `type: "message"` record.
type persistedMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlock is one entry of an assistant message's content array. Only
// `text` blocks are completion-relevant; toolCall/toolResult blocks are
// deliberately ignored (they can echo the marker without the agent having
// finished).
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// assistantTextFromRecord returns the assistant-authored text in one record,
// or "" when the record carries none.
//
// Deliberately narrow: type=="message" AND role=="assistant" AND text blocks
// only. `custom`/dcp-state, user messages (which contain the brief, including
// the marker as an instruction!), and toolCall arguments are all excluded.
func assistantTextFromRecord(rec *transcriptRecord) string {
	if rec.Type != "message" || len(rec.Message) == 0 {
		return ""
	}
	var m persistedMessage
	if err := json.Unmarshal(rec.Message, &m); err != nil {
		return ""
	}
	if m.Role != "assistant" || len(m.Content) == 0 {
		return ""
	}
	// Shape A: content is a bare string.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// Shape B: content is an array of typed blocks.
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// scanTranscript streams the transcript from `fromOffset`, calling fn with the
// assistant text of each record. It returns the offset just past the last
// COMPLETE record consumed, so a caller can resume incrementally without
// re-reading. Only whole records are consumed: a trailing record without its
// newline is a torn write and is left for the next pass.
func scanTranscript(path string, fromOffset int64, fn func(assistantText string) bool) int64 {
	f, err := os.Open(path)
	if err != nil {
		return fromOffset
	}
	defer func() { _ = f.Close() }() // read-only handle; close cannot lose data

	if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
		return fromOffset
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	consumed := fromOffset
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			consumed += int64(len(line)) + 1
			continue
		}
		var rec transcriptRecord
		if err := json.Unmarshal(line, &rec); err == nil {
			if !fn(assistantTextFromRecord(&rec)) {
				return consumed
			}
		}
		consumed += int64(len(line)) + 1 // +1 for the newline
	}
	if sc.Err() != nil {
		// Torn tail or oversized record: consumed only counts whole records, so
		// resuming from it re-reads at most the incomplete remainder.
		return consumed
	}
	return consumed
}

// TranscriptAssistantText returns the assistant text from the last `window`
// bytes of a transcript, newest records last. `window <= 0` uses
// TranscriptWindow.
func TranscriptAssistantText(path string, window int) string {
	if window <= 0 {
		window = TranscriptWindow
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return ""
	}
	from := max(fi.Size()-int64(window), 0)
	var b strings.Builder
	scanTranscript(path, from, func(text string) bool {
		b.WriteString(text)
		return true
	})
	return b.String()
}

// TranscriptContains reports whether `marker` appears in the transcript's
// assistant-authored text (ADR-0011). This is the completion surface for the
// marker gate: cumulative and append-only, so a marker emitted in ANY round is
// found — which is exactly what the truncated run log could never do.
//
// An empty marker NEVER matches. strings.Contains(s, "") is true for every
// non-empty s, so an unguarded empty marker would declare any finished job
// done. The caller gates on a non-empty marker too, but the primitive is safe
// standalone.
func TranscriptContains(path, marker string) bool {
	if marker == "" {
		return false
	}
	return textEmitsMarker(TranscriptAssistantText(path, TranscriptWindow), marker)
}

// textEmitsMarker reports whether assistant text EMITS the marker, as opposed to
// merely containing the string.
//
// It is a line-anchored match, not strings.Contains, because the two are
// indistinguishable by substring and only one of them means the work is done.
// This caused a real false-positive completion on mealime-userrecipes
// (2026-10-05): the job reported `state: done`, `marker_found: true`, report
// present — while T6-T9 were unimplemented. The agent had written:
//
//	"ALL_MEALIME_USERRECIPES_DONE deliberately **not** emitted: T6-T9 are incomplete."
//
// A bare substring matched that sentence, the gate closed, and four rounds of
// real work were silently discarded: a done job stops looping, so the next round
// never ran. The agent was REPORTING the incomplete state, and the supervisor
// read it as completion.
//
// The rule is NOT "the marker occupies a line by itself". Real transcripts do
// not look like that: ADR-0011's own regression tests carry
// "Done. ALL_MEALIME_ROOMUX_DONE" and "still complete: ALL_MEALIME_ROOMUX_DONE",
// and a real mealime-roomux transcript emitted "## ALL_MEALIME_ROOMUX_DONE". A
// position rule rejects markers that genuinely completed the work.
//
// The rule is about NEGATION and QUOTATION, which is what actually
// distinguishes the false positive:
//
//	ALL_DONE                                  -> completion
//	ALL_DONE deliberately NOT emitted: ...     -> a report of incompletion
//	the brief says to emit ALL_DONE            -> explaining the marker
//	don't emit the marker until ...            -> deferring it
//
// So a mention counts UNLESS the same line declines or defers it. That is
// deliberately one-directional: a missed marker costs one extra round and is
// recoverable, while a false "done" silently discards the remaining work.
func textEmitsMarker(text, marker string) bool {
	if marker == "" {
		return false
	}
	var prev string
	for line := range strings.Lines(text) {
		if strings.Contains(line, marker) && !mentionIsRefused(line, marker, prev) {
			return true
		}
		prev = line
	}
	return false
}

// mentionIsRefused reports whether a line that CONTAINS the marker actually
// declines or defers emitting it. Scoped to the text around the marker, so a
// negation elsewhere in the same long paragraph does not veto a real emission.
//
// prev is the preceding line. An agent often sets the refusal up first and then
// writes the bare marker — "I will not emit the marker:" followed by
// "ALL_DONE" on the next line — so a refusal on the line BEFORE counts too.
// Without it the most explicit refusal in the transcript was the one shape that
// did not work.
func mentionIsRefused(line, marker, prev string) bool {
	idx := strings.Index(line, marker)
	// Window: from the start of the line to a little past the marker, so
	// "deliberately NOT emitted" AFTER it is caught, plus a short look back for
	// "do not emit ALL_DONE" BEFORE it.
	lo := max(idx-64, 0)
	hi := min(idx+len(marker)+48, len(line))
	window := line[lo:hi]
	// Include the tail of the previous line, so "…not emit the marker:" on the
	// line above still reaches this window.
	if prev != "" {
		tail := prev
		if len(tail) > 48 {
			tail = tail[len(tail)-48:]
		}
		window = tail + "\n" + window
	}

	for _, re := range markerRefusalRes {
		if re.MatchString(window) {
			return true
		}
	}
	// An explicit "not emitted" / "do not emit" anywhere in the window.
	if markerRefusalRe.MatchString(window) {
		return true
	}
	return false
}

// markerRefusalRes are the ways an agent says "I am NOT emitting this", captured
// only in a window around the marker mention. Every entry is a NEGATION or a
// deferral, so a plain completion sentence can never match one.
var markerRefusalRes = []*regexp.Regexp{
	// "ALL_DONE deliberately NOT emitted", "marker was not emitted".
	// Markdown emphasis may sit INSIDE the phrase — an agent writes
	// "ALL_DONE **not** emitted" — so allow emphasis between the words.
	regexp.MustCompile(`(?i)\b(?:not|isn't|wasn't|never)\b[\*_` + "`" + `\s]{0,4}emitted\b`),
	// "deliberately NOT done", "explicitly NOT emitted"
	regexp.MustCompile(`(?i)\b(?:deliberately|explicitly|intentionally)\s+(?:\**\s*)?(?:not|never)\b`),
	// "do not emit", "don't emit", "will not emit", "without emitting", and the
	// same with Markdown emphasis inside the phrase ("will **not** emit").
	regexp.MustCompile(`(?i)\b(?:do\s+not|don'?t|will\s+not|won'?t|never|without|shall\s+not)\b[\*_` + "`" + `\s]{0,4}(?:\w+[\*_` + "`" + `\s]{0,4}){0,2}emit\b`),
	// "T6-T9 are NOT done", "remain incomplete" next to the marker
	regexp.MustCompile(`(?i)\b(?:are|is|remain|remains)\s+(?:\**\s*)?(?:not|isn't|wasn't)\s+(?:done|complete|finished)\b`),
	// "the brief says to emit ALL_DONE" — quoting instructions, not emitting
	regexp.MustCompile(`(?i)\b(?:brief|prompt|instructions?|rule)\s+(?:says?|says\s+to|requires?|instructs?)\b`),
	// "should emit", "must emit" — describing a rule
	regexp.MustCompile(`(?i)\b(?:should|must|when\s+to)\s+emit\b`),
}

// markerRefusalRe is the bare "not emitted" form, kept separate so the intent is
// readable at the call site.
var markerRefusalRe = regexp.MustCompile(`(?i)\bnot\s+emitted\b`)

// TranscriptWatcher incrementally tails a transcript and latches when the
// marker first appears in assistant text. It is the streaming form of
// TranscriptContains: bytes are consumed once, in order, and never re-read.
//
// The latch is STICKY. Once the marker has been seen, later truncation or
// buffer rotation cannot clear it — which is the property the run-log path
// lacked and the reason a finished job could livelock forever.
type TranscriptWatcher struct {
	path      string
	marker    string
	offset    int64
	seen      bool
	seenAtOff int64
}

// NewTranscriptWatcher starts watching `path` for `marker`, resuming from the
// file's current end (it does not replay history: a round's watcher only cares
// about what pi says from now on). Pass a non-empty `from` to seed the offset,
// e.g. to skip records already consumed.
func NewTranscriptWatcher(path, marker string, from int64) *TranscriptWatcher {
	w := &TranscriptWatcher{path: path, marker: marker, offset: from, seenAtOff: -1}
	if fi, err := os.Stat(path); err == nil && from == 0 {
		w.offset = fi.Size()
	}
	return w
}

// Poll consumes whatever has been appended since the last call and returns
// true once the marker has been seen in assistant text. Cheap to call often:
// after the marker trips it is a sticky true and does no I/O.
func (w *TranscriptWatcher) Poll() bool {
	if w == nil || w.seen || w.marker == "" || w.path == "" {
		return w != nil && w.seen
	}
	// A file that shrank (compaction rotated it, or the round truncated it)
	// would leave the offset past EOF and the watcher blind forever. Re-sync
	// to the new end instead: better to miss a marker than to hang on a
	// silently-dead offset. The gate's other check (final_report) still has to
	// pass, so a missed marker cannot fake a done.
	if fi, err := os.Stat(w.path); err == nil && fi.Size() < w.offset {
		w.offset = 0
	}
	w.offset = scanTranscript(w.path, w.offset, func(text string) bool {
		// The SAME rule as TranscriptContains, not strings.Contains. The gate
		// reads this sticky latch FIRST, so a watcher using the looser test
		// would re-introduce the quoted-marker false positive through the latch
		// even though the cumulative scan rejects it.
		if textEmitsMarker(text, w.marker) {
			w.seen = true
			w.seenAtOff = w.offset
			return false // stop consuming; we have what we came for
		}
		return true
	})
	return w.seen
}

// Seen reports whether the marker has been latched.
func (w *TranscriptWatcher) Seen() bool { return w != nil && w.seen }

// Offset is the next byte offset the watcher will read. After the marker
// trips it stops advancing, since there is nothing left to look for.
func (w *TranscriptWatcher) Offset() int64 { return w.offset }
