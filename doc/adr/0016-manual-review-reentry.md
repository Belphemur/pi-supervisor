# ADR 0016 — manual review re-entry on a done job

Date: 2026-10-06
Status: Accepted
Author: Antoine Aflalo (Belphemur), Hermes Agent

## Context

ADR-0012's campaigns have one entry: the AUTO trigger, which fires in the
completion gate's tail (`done → reviewing` on the marker + open PR). A MANUAL
campaign — `review <job> --pr N` followed by starting the job — had three
defects that together made re-running a review campaign on a job whose build
phase already finished impossible without a hand dance. The live case was
`mealime-rebase54` (flambette PR #54, 16 open bot threads): the operator had
to delete `~/.pi/supervisor/state/<job>.json` by hand, restart the daemon
(wiping the just-armed campaign), rewrite the job's marker to a fresh string,
rewrite the cont prompt to suppress the old marker, then arm and start — five
hand steps the daemon should own, and exactly the "do not seed state to force
a code path" move the docs forbid.

The three defects:

1. **`Start` refused a done job unconditionally** (`already done; clear state
   to rerun`) — even with a campaign armed. Arming is allowed on a done job
   (`StartReview` only refuses an ACTIVE job), but the armed campaign could
   never get a round loop.
2. **`Start` set state `"running"` even with a campaign armed.** The loop
   computes `reviewing` from `r.review != nil && state == "reviewing"`, so a
   manual campaign never owned the loop: the job's MaxRounds applied instead
   of the campaign's (invariant 23 broken for manual campaigns), and
   `reviewGate` — the only exit that checks "0 open threads AND CI pass" —
   never ran. The campaign could not terminate itself.
3. **The marker gate had no live-campaign guard.** The sticky `MarkerSeen`
   latch (durable in state) plus the deliberately cumulative
   `TranscriptContains` scan (ADR-0011: byte 0 of the transcript) find the
   PREVIOUS campaign's marker in a resumed session, so the job closed `done`
   at the end of round 1 — before the campaign did anything. This is what the
   operator's log recorded: "job closed 'done' at round 1 instantly (marker
   latch)".

## Decision

**A done job with an ARMED campaign is the supported manual re-entry:**
`review <job> --pr N` — one command: arming launches the campaign directly (2026-10-07 owner correction: the arm-then-start two-step read as a dead button). No state-file
deletion, no daemon restart, no marker rewrite.

- **`Start` refuses `done` only when NO campaign is armed.** With a campaign
  armed it proceeds and sets state `reviewing` — the same state the auto
  handoff uses — so the loop's existing `reviewing` branch applies from round
  1: the campaign's MaxRounds governs (invariant 23) and `reviewGate`
  evaluates between rounds. `Start` also stamps `r.review.round` from the
  state round (mirroring the auto handoff) and emits a non-terminal
  `reviewing` event, so a watch client sees the handoff.
- **The marker gate is INERT while `reviewing`.** A review round has no
  marker of its own (ADR-0012: "there is no marker in a review round"); its
  exits are `reviewGate`'s `review_done` and the budget's `review_exhausted`.
  The AUTO trigger keeps the gate: there `r.review` is still nil — the
  handoff that CREATES the campaign lives inside that gate.
- **A campaign is spent at its terminal moments.** `finish()` clears
  `r.review` (the exhausted path's snapshot-before-finish ordering is now
  actually true — it previously relied on finish NOT clearing), and
  `reviewGate`'s clean close clears it too. A later `start` after
  `review_done` is refused as done, as it should be. Operator stops do NOT
  finish (Stop writes its state directly), so a stopped campaign stays armed
  and resumes on the next start — consistent with "daemon restarts adopt jobs
  as resumable".
- **The gate's GitHub reads go through a `reviewReader` interface** (mirroring
  the `listOpenThreads` var): production satisfies it with `*review.Client`,
  tests stub it. No behavior change.

## Consequences

- The rebase54-style flow collapses from five hand steps to two commands, and
  the state file's round counter, review baseline, and session pin all
  survive — the hand dance destroyed them.
- A plain `start` on a done job still refuses. "Already done" remains the
  honest answer when the operator has NOT armed a campaign.
- A stale `reviewing` state (daemon restarted mid-campaign; the campaign is
  in-memory only) self-heals: `start` with no campaign sets `running`, and
  the documented remedy is re-arming `review <job> --pr N`.
- The old marker stays latched on state (`marker_found: true` during the
  campaign). That is honest — the marker WAS seen, by the previous campaign —
  and harmless once the gate is inert mid-campaign.

## Non-goals

- Persisting the campaign across daemon restarts. It is in-memory by design;
  the re-arm command is the recovery path.
- Re-scoping `c.round` accounting (the displayed "review round N/M" comes
  from the handoff stamp; per-round increments are a cosmetic follow-up).
- Changing `TranscriptContains`'s cumulative semantics (ADR-0011). The fix
  keys on the loop's `reviewing` state, not on weakening the scan.