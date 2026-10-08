# ADR-0011: The session transcript is the completion surface (streamed), not the run log

## Status

Accepted. Implementation follows this ADR.
Superseded IN PART by [ADR-0020](0020-report-is-the-completion-signal.md):
the transcript text-block rule stands, but the "marker AND report" done
conjunction is replaced — the report alone closes done, and the marker asks
for the report (twice) instead of gatekeeping it.

## Context

Job `mealime-roomux` (PR #43) ended `fatal` after **14 rounds** with
`last_diag: "round cap reached without marker"` — while the work was actually
finished and merged, with 18/18 CI green.

The evidence that this was a harness bug, not an agent failure:

| Signal | Value | Meaning |
|---|---|---|
| `last_rc` | `0` | pi's round exited cleanly |
| `pr_url` | `https://github.com/Belphemur/flambette/pull/43` | the PR regex matched — pi *did* print the PR URL |
| marker in the 1.7MB session transcript | **43 occurrences** | pi emitted `ALL_MEALIME_ROOMUX_DONE` repeatedly |
| marker in `/tmp/pi_mealime-roomux_run.log` | **0** (362 bytes total) | the gate's input never contained it |

### Root cause

`supervisor.round()` truncates the run log at the start of every round:

```go
_, runlog, _, _ := job.Paths(j.Name)
_ = os.Truncate(runlog, 0)   // supervisor.go:768
```

and the marker gate then reads only that file:

```go
if j.Marker != "" && job.Exists(j.FinalReport) && job.RunlogContains(job.Runlog(name), j.Marker) {
    // supervisor.go:656 — declare done
}
```

So the gate reads a **per-round, destroyed-every-round** artifact while asking
a **cumulative** question ("has this job ever signalled completion?"). Once
the round that carried the marker ended, the evidence was gone. Every
subsequent round re-ran the full prompt, and each one could only re-emit the
marker into a log that the *next* round would truncate — a livelock where the
job can never satisfy its own gate. Rounds 6–14 were pure waste.

Two secondary defects in the same path:

1. **`RunlogContains` reads only the last 4KB.** Even within a single round, a
   marker followed by >4KB of trailing output is missed. `strings.Contains`
   over a fixed tail window is not a completion test.
2. **The gate only runs at round end.** Detection is a post-hoc classification,
   so the earliest possible signal — the marker appearing mid-round — buys
   nothing.

### Why not just widen the run-log window?

Rejected. The run log is `os.Truncate`d per round; a larger window makes the
window/boundary bug *less* likely without fixing the *destruction* bug, which
is the one that actually bit. Widening would also keep re-reading a log that is
concurrent with a live writer. The artifact itself is wrong for this question.

## Decision

### 1. The session transcript JSONL is the completion surface

The transcript is append-only, cumulative, survives every round boundary, and
is the durable record of what pi actually said. It is the correct artifact for
a cumulative "did this job ever finish?" question.

New primitives in `internal/job`:

- `ExtractAssistantText(path string, window int) string` — reads the **last
  `window` bytes** and returns the reconstructed assistant text. JSONL event
  records are correlated back into message text, so a marker **split across
  streamed `text_delta` records** is reassembled into one searchable string.
  Line-oriented grep cannot do this; each delta is its own JSON line.
- `TranscriptContains(path, marker string) bool` — `marker != ""` **and** the
  marker occurs in `ExtractAssistantText`. An empty marker never matches
  (`strings.Contains(s, "")` is true for every non-empty `s`, which would
  declare any finished job done — the primitive is safe on its own, not only
  because the caller happens to guard).

The window default (256KB) matches `TailLines` and is a **performance** bound,
never a correctness bound: the boundary-straddling test proves a marker split
across the window edge is still found, because reconstruction happens over the
whole window's text rather than per-line.

### 2. Stream it, and keep parsing as it streams

Per the requirement that the completion signal be detected *while* the round
runs, not classified after:

- A new per-round watcher tails the transcript **incrementally from a byte
  offset**, parsing only newly appended bytes and folding them into a rolling
  text buffer. It never re-reads from the start, and it never re-reads a
  window it has already consumed.
- On the first frame whose reconstructed text contains the marker, the watcher
  records a **latch**: `markerSeen` flips true and is **sticky for the rest of
  the round**. Later truncation, buffer rotation, or a fresh run log cannot
  clear it.
- The latch is the signal. When it trips, the round is classified as complete
  at the next round boundary (or immediately, if the caller is polling) —
  `done` requires **both** the marker latch **and** `final_report` existing, the
  same two-condition rule as today. The latch only replaces *where the marker is
  read from*; it does not weaken the gate.

`Result` gains a `MarkerSeen bool`, set from the latch. The supervisor's gate
becomes:

```go
if j.Marker != "" && markerSeen && job.Exists(j.FinalReport) { /* done */ }
```

`RunlogContains` is **retained** as a fallback for rounds where no transcript is
available (a session that failed to launch, a pre-ADR-0011 job still being
adopted). The run log is a strictly weaker signal — cumulative in neither
scope nor time — so it can only ever add a `done`, never mask a miss. A job
that completes *only* via the run-log path is logged as `done_source=runlog` so
the weaker path stays visible instead of silently masking the real bug class.

### 3. Detection no longer waits for round end

Because the latch is set from the streaming watcher, the supervisor sees the
marker while the round is still live. This makes the completion signal
available to `status` (`marker_found`) and to `watch` mid-round, instead of
only appearing in the next classification.

## Consequences

- **The livelock is gone.** A job that emitted its marker is done on the next
  gate, no matter how many rounds came after or how much trailing output
  followed the marker.
- Cost per round: one incremental tail of the transcript, offset-tracked. The
  round loop already tails this same file for CI-stall and empty-turn
  detection (`watchCIStalls`, `watchEmptyTurn`), so this is one more consumer
  of an already-watched file, not a new I/O pattern.
- `MarkerFound` in `status` becomes **truthful** mid-round instead of only
  reflecting a post-hoc classification.
- A job whose agent genuinely never emits the marker is unaffected: it still
  runs to `MaxRounds` and ends `fatal`. The fix does not create false
  positives; it removes false negatives.
- Transcripts grow unbounded (the real one was 1.7MB for 14 rounds). The
  windowed read keeps per-round cost flat regardless of size. Transcript
  rotation is **not** implemented here — same explicit non-goal as ADR-0010's
  quarantine rotation.

## Non-goals

- Treating the marker in *any* line as sufficient without the `final_report`
  existence check. The two-condition rule is preserved.
- Reconstructing full conversation semantics, tool calls, or reasoning. Only
  assistant **text** is reassembled; that is what the marker lives in.
- Detecting completion from tool calls or PR state. The marker remains the sole
  completion token.