# ADR 0004 — CI-stall awareness: detect the answer-code-review park and cap the retries

Date: 2026-10-02
Status: Accepted
Author: Antoine Aflalo (Belphemur), Hermes Agent

## Context

The endurance-governor campaign burned its round cap waiting on CI: round 9
said "10 checks running. Let me wait for CI and the re-review to land", then
rounds 10–12 each consumed the full 1800s timeout parked on the same GitHub
checks, and the job went `fatal` on the round cap — hours lost doing nothing.

The owner's PR workflow always runs `/answer-code-review`, so "parked on CI /
review" is a *known, recurring* shape, not an anomaly. The supervisor should
recognize it from the session transcript as it streams, and decide how many
retries it tolerates before (a) telling the session to finish with the report
and (b) closing the run as a **failure to finish the review loop**.

## Decision

**Stream-parse the session JSONL for CI-wait markers, count quiet parks, and
act at the cap.**

- **Detection (`internal/stall`):** a per-round detector tails the session
  JSONL from its current end (historical content — the round-1 brief echo,
  older rounds — never arms it). It scans new raw lines for markers of the
  CI/review path: "waiting for/on CI|checks|review|workflow|tests",
  `gh pr checks|reviews|status`, `answer-code-review`, "re-review",
  "checks running|pending|queued", "review loop". Raw-line regex over the
  JSONL stream is deliberately chosen over structured parsing: the marker is
  prose the agent emits, and schema drift must not break detection.
- **Stall = marker armed + transcript quiet.** A marker only *arms* the
  detector; the stall fires when the transcript has grown nothing for
  `ci_stall_idle_s` (default 300s). "Alive but idle" is exactly what waiting
  on CI looks like in the transcript; a long build emits tool output and
  keeps resetting the quiet timer. After a stall fires, the detector
  re-arms, so park → wake → park counts as multiple stalls.
- **Counting:** each stall increments `state.ci_stalls` (persisted,
  cumulative across rounds) and emits a non-terminal `ci_stall` watch event
  ("stall N/cap — waiting on CI: <marker>"). The round is left running —
  that is the "retry" being allowed.
- **At `ci_stall_cap` (default 3, 0=default):**
  1. the supervisor writes an `abort` + `prompt` frame pair to the job's
     control file — the same channel `pi_control.py --interrupt` uses —
     telling the agent: *stop polling CI, finish the report now (what
     landed, what failed, what the operator must check); this run is being
     closed as a failure to finish the review loop*;
  2. it marks the job `fatal` with that reason and emits the terminal
     `fatal` event, so the session that armed the watch wakes with
     "THE RUN IS OVER" and the review-loop failure diagnosis.
  The current round is NOT killed: the client stays alive to deliver the
  abort+prompt, so the report turn actually happens. When that final turn
  ends, `loop()` checks state: if the marker+final-report gate then passes,
  the run upgrades to `done` (the report was written); otherwise the
  `fatal` stands.
- **Scope:** the watcher runs only in rounds where the session path is
  already captured (round 2+); round 1 has no transcript to tail yet and
  keeps the plain timeout behavior.
- **Config:** `ci_stall_cap` (0 → 3) and `ci_stall_idle_s` (0 → 300) on the
  job; tests use small values. Defaults are deliberately patient: three
  5-minute parks ≈ 15 minutes of CI waiting before intervention, versus the
  3×1800s the round cap previously burned.

## Alternatives considered

- **Structured JSONL parsing** (assistant-message content blocks): more
  precise, but couples the daemon to pi's entry schema for a signal that is
  fundamentally prose. Rejected for now; the regex can move into a parser
  later without changing the supervisor contract.
- **Pure transcript-idle detection** (no markers): simpler, but cannot
  distinguish "parked on CI" from "running a long build/tool call", and
  would false-positive on legitimate long tool runs.
- **Letting pi's own model-fallback/timeout handle it:** it cannot — the
  agent believes waiting is correct behavior; only an outside observer with
  the retry budget can overrule it.

## Consequences

- The supervisor self-steers via the control file — the same trust path as
  operator `steer`; no new privilege.
- `ci_stalls` is visible in `pi-supervisor status`, so the operator sees how
  close a run is to the cap.
- The finish-report turn is unsupervised by further rounds (the job is
  already fatal); if the agent ignores the instruction, nothing is lost —
  the run was failing anyway.
- Regex markers can miss unusual phrasings; the cost of a miss is the old
  behavior (burn the round timeout), never a wrong intervention.
