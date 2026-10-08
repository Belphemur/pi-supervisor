# ADR-0020: The final report is the completion signal — marker asks for the report, twice

Date: 2026-10-08
Status: Accepted — supersedes the ADR-0011 completion conjunction

## Context

ADR-0011 made the session transcript's assistant TEXT blocks the completion
surface: `done` requires the marker seen in an assistant message AND the
final report on disk. The transcript-only rule is correct — it is what killed
the mealime-roomux run-log livelock and the mealime-userrecipes quoted-marker
false positive — but the marker AND report conjunction has a failure mode the
real position-restore run (2026-10-08, PR #203) hit exactly:

- The agent completed all 7 tasks, opened the PR, ran every gate green, and
  wrote the final report to `/tmp/pi_position_restore_final_report.md` —
  ENDING THE REPORT FILE with the marker line.
- It interpreted the brief's "end your LAST assistant message with the marker
  line" as "end the report with it". The marker appears in the transcript 20
  times — brief quotes, a compaction summary, a `write` toolCall's arguments
  (the report content), and 3 assistant THINKING blocks ("The final report is
  complete and ends with the marker TTF_POSITION_RESTORE_DONE") — but NEVER
  as an assistant text block.
- The gate behaved per spec and never closed. The agent then spent rounds
  7-12 in 2-3 second "Current state is complete. Let me verify one more
  time:" loops while the supervisor counted to the round cap. The job died
  `fatal` with "round cap 12 reached without marker" — technically true,
  operationally useless: the marker was never missing, it was in the wrong
  place, and nothing told the operator that.

Two defects, neither in the transcript rule itself:

1. **No surface diagnosis.** When the marker exists on non-text surfaces but
   not as text, the fatal says "without marker" and the operator re-derives
   by hand what the daemon already knew.
2. **No recovery path.** An agent that believes it is done but wrote the
   marker in the wrong place is not making progress; looping to the cap is
   the only outcome. The daemon never ASKS for the one missing artifact.

## Decision

**The fully written final report is the completion signal. The marker is the
completion INTENT.**

At the round boundary (build phase), the gate evaluates, in order:

1. **Report complete → done.** The final report exists, does not declare
   itself incomplete (the existing `ReportDeclaresIncomplete` gate, which
   continues the loop instead), and was written during THIS run — mtime not
   before the run's `StartedAt` floor. The marker is no longer required.
   The floor is what keeps a report left behind by a previous run from
   closing a `restart --fresh` done at round 1; a fresh restart resets
   `StartedAt`, the marker latch, and the ask counter, because it is a NEW
   RUN.
2. **Marker on any surface + report missing → ask for the report.** "Marker
   on any surface" = assistant text (latch / cumulative transcript scan /
   run log), OR the report file's last line, OR any non-text transcript
   surface (thinking block, toolCall argument, toolResult, user quote,
   compaction summary). The daemon arms an abort+prompt into the NEXT live
   round: "write the final report to <path> NOW … then end your assistant
   message with the marker line." Each actual delivery increments
   `state.report_steers` (persisted; surfaced as `report_steers` in status).
   The pending flag is in-memory and re-armed by the next boundary if a
   delivery never landed — the count only ever counts real asks.
3. **Two asks, then fatal.** At the boundary after the second ask, if the
   report still is not there, the job closes `fatal`:
   "final report <path> still missing after 2 report requests". A third ask
   burns rounds for nothing; the fatal names the artifact so the remedy is a
   copy-paste.
4. **Marker nowhere, report missing, or NO report path configured →
   unchanged.** A job without `final_report` has nothing to ask for, so the
   marker alone never triggers a request; the job keeps looping to its round
   cap. The cap diagnosis is now enriched (below).

The report-steer delivery is a round-scoped goroutine (`deliverPendingReportSteer`)
alongside the marker/CI-stall/empty-turn watchers: it waits for the client to
publish its pid, then reuses `interruptWith` — the same abort+prompt wire
shape the CI-stall cap uses. No new delivery mechanism.

## Cap diagnosis: name the surfaces

The round-cap switch shares ONE marker funnel with the gate
(`markerSeenNow`: sticky latch ∪ cumulative transcript scan ∪ current run
log — the two pre-funnel copies were a DRY violation waiting to drift). When
the cap is reached without a text marker, the diagnosis classifies where the
marker DOES live (`ScanMarkerSurfaces` + `ReportEndsWithMarker`) and says so:

> round cap 12 reached without marker — but the marker WAS written: 3
> assistant thinking block(s), 1 toolCall argument(s), the final report FILE
> ends with it; only an assistant TEXT block counts (ADR-0011/ADR-0020):
> steer the agent to end a message with the marker line

"Without marker" is reserved for markers that are genuinely nowhere.

## What does NOT change

- Only assistant TEXT blocks satisfy the completion surface (invariant 15).
  Thinking blocks, toolCall arguments, and compaction summaries still never
  close a job done — they now only trigger the report request. The
  mealime-userrecipes false-positive protection (negation-aware
  `textEmitsMarker`) is untouched.
- The `report_declares_incomplete` path still refuses done and keeps looping.
- Review campaigns (ADR-0012/0016) are unaffected: while `reviewing`, the
  marker gate is inert and the campaign's own gate governs.
- The auto-review trigger still runs from the gate's tail; a report-driven
  done arms it exactly as a marker-driven one did.

## Consequences

- A position-restore-shaped run now closes `done`: the report — the artifact
  the operator actually reads — is complete, and the marker-in-thinking
  footnote is invisible.
- An agent that signals completion and ignores two report requests dies
  fast with an actionable fatal instead of burning `max_rounds` at 2 s per
  round.
- `report_steers` in `status` makes the ask loop observable mid-run.
- Risk: a "fully written" report describing UNFINISHED work still closes
  done. The existing mitigations are the declares-incomplete scan and the
  task-completion watch (ADR-0014); the owner has accepted the residual risk
  in exchange for not re-deriving completion by hand.

## References

- Live session: `--home-balor-workspace-eink-crosspoint-worktree-progress-anchor--/2026-10-08T01-47-44-152Z_01a11931-….jsonl`
  (marker: 20 hits, 0 in assistant text; 4 in assistant role records — 3
  thinking, 1 toolCall argument).
- Regression: `capdiag_replay_test.go` replays the problematic session
  through a fake pi (no real pi) and asserts the new verdicts.
