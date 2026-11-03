# ADR-0024: Job-def contract is enforced at load, and the CLI writes the def

## Status

Accepted (2026-11-03)

## Context

A job definition is a hand-authored JSON file in `~/.pi/supervisor/jobs/`, and
every layer of the pipeline was polite about a wrong one:

- `job.Load` used plain `json.Unmarshal`, so an invented key was SILENTLY
  DROPPED. Live (mem-claims-analysis, 2026-11-03): a def written with
  `job`/`brief_path`/`cont_path`/`report` loaded as a struct of empty strings,
  the daemon's own `Save` normalized the file on disk to those empty strings,
  and every round died instantly with `prompt unreadable: empty prompt` before
  pi ever spawned. Diagnosis cost half an hour because the proof sat in the
  orchestrator log (`/tmp/pi_<job>_orchestrator.log`) while the greps went to
  the journal and the runlog.
- `LoadJobs` skipped an unparseable def with a bare stderr line that no
  operator surface reports: `reload` still exited 0, so the defect surfaced
  hours later as a fatal round instead of at write time.
- Nothing structurally validated a def: empty `cont` (resume rounds die), an
  empty `marker` (the completion gate can never match — livelock to
  `max_rounds`), or a zero `max_rounds` were all loadable.

The owner's directive: enforce the required fields, log the errors clearly,
and replace the hand-written-JSON dance with a client that takes the job's
parts as clearly-documented args.

## Decision

1. **Strict decode, all keys named.** `job.Load` diffs the file's keys against
   the `Job` struct's json tags (computed via `reflect`, so the gate cannot
   drift from the model) and REJECTS the def naming EVERY unknown key, each
   with a `did you mean` hint when it matches a known field synonym
   (`brief_path` → `brief`, `report` → `final_report`, …). A strict
   `DisallowUnknownFields` decode runs after the key check as a backstop.
   Unknown keys are an error, never a silent drop.
2. **Structural validation at load.** `job.Validate` refuses a def with an
   empty `name`/`brief`/`cont`/`marker`/`worktree` or non-positive
   `max_rounds`/`timeout_s`. `LoadJobs` refuses such a def, journals
   `WARN job_def_invalid file=<name> reason=…`, and returns the aggregate
   error — so `pi-supervisor reload` exits 1 naming each bad file. Valid defs
   still load in the same pass. Daemon BOOT keeps the forgiving shape: it
   logs the failure and still becomes ready (a bad def must never
   crash-loop the unit).
3. **`pi-supervisor job create|list|show` (CLI, no daemon needed).** `create`
   writes `<name>.json` from typed cobra flags — the canonical field names are
   the flag names, so the invented-key class cannot be constructed through
   the surface. Required flags (`--brief --cont --marker --worktree`) are
   cobra-enforced (exit 2); the brief/cont/review_brief files must exist
   (`final_report` is exempt — the agent writes it during the run); an
   existing def is refused without `--force`; stdout names the next commands
   (`reload && start`). `list` prints every def and marks INVALID ones with
   the reason; `show` prints the normalized JSON.
4. **Reload behavior change (deliberate).** A corrupt/invalid def now FAILS
   the reload (exit 1, message names the file) instead of being a silent
   skip. The silent skip was the outage class: nothing ever told the operator.
   Partial load semantics are unchanged — good defs load, bad defs are
   refused individually.

## Consequences

- A typo'd def fails at reload, in seconds, with the exact key named — not
  three hours later as a fatal round with an empty runlog.
- Job defs on disk are now trustworthy: if the daemon loaded it, the required
  fields are present and non-empty.
- The next outage of this class has THREE surfaces naming the cause: the
  reload exit, the journal `job_def_invalid` line, and `job list`.
- `job create` does not talk to the daemon: defs load on `reload` exactly as
  before (the daemon reads definitions from disk only on reload).

## Non-goals

- No schema versioning or migration machinery: the Job struct evolves in
  place; defaults live in `Load`.
- No validation of skill paths, `provider`/`model`, or `pi_bin` existence:
  pi resolves those at spawn time and the failure surfaces in the
  orchestrator log (and the runlog now carries the spawn argv).
- No daemon-mediated create: the CLI writes the file directly; adding a
  socket round-trip would couple def creation to daemon availability for
  no enforcement gain (the daemon validates on reload anyway).