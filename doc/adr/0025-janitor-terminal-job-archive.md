# ADR-0025: Janitor — automatic archival of terminal jobs

## Status

Accepted (2026-11-09). Directed by the owner in chat: sessions (jobs) that
ended in failure or success must be archived automatically so they are never
reloaded, sweeping at daemon start and then every hour, the interval
configurable with 1h as the default, implemented as a daemon goroutine.
Two judgment calls this ADR adds beyond that directive are flagged in §4 and
can be vetoed before the implementation lands.

Reconciled 2026-11-09 (review round, 14 threads accepted, none rebutted):
session-quarantine-before-def-move, the archival critical section
(revalidation + targeted runner removal under held locks), the archived
job's own `state/<name>.json` moving with it, link-then-remove no-replace
moves, review-baseline exclusion, `job_archived` made terminal, the anchor
stat moved out of the locks, and the interval floor. Each is folded into
the section it amends below.

## Context

A job def is a JSON file in `~/.pi/supervisor/jobs/` and `LoadJobs`
(internal/supervisor/supervisor.go) adopts every file it finds — a subdir is
skipped by construction, which is how the operator's manual convention works:
dead defs are `mv`-ed into `jobs/_archive/` (pi-supervisor-dev-quirks skill;
"mv back restores"). Three costs accumulate because nothing does this
automatically:

- Terminal jobs — `done`, `fatal` — keep their defs forever. Months of
  campaigns pile up in `status`/`job list` and in the jobs dir itself.
- A finished job's session JSONL stays adoptable: nothing stops a stale
  resume or a careless `start` from re-attaching a transcript whose work
  ended days ago. ADR-0010 gave `restart --fresh` the `_archived-stale/`
  quarantine for exactly this, but only that path triggers it.
- Archived-by-hand depends on a human remembering. The failure it prevents
  (a failed or successful old job being reloaded and re-run) is silent.

The daemon already has the right primitives: move-only archival targets on
both layers, an event funnel, and a background-goroutine pattern
(`go sup.Monitor(stop)`, `go sup.ExpireAcks(stop)` in `runDaemon`).

## Decision

**One janitor goroutine in the daemon**, started in `runDaemon` next to
`Monitor` and `ExpireAcks`. It sweeps once immediately after jobs load, then
on a ticker.

1. **Configuration** (viper flat keys, same precedence stack as ADR-0008:
   env `PI_SUPERVISOR_*` > `~/.config/pi-supervisor/config.yaml` > default;
   no flags — there is no per-invocation use for them):

   | Key | Default | Meaning |
   |---|---|---|
   | `janitor_enabled` | `true` | false = goroutine exits after nothing |
   | `janitor_interval` | `1h` | `viper.GetDuration` sweep cadence, floored at 1m |
   | `janitor_archive_after` | `168h` | terminal age before archival |

   Defaults are registered with `viper.SetDefault` in `initConfig`; the
   values are read in `runDaemon` the way `socketPath()` reads `socket` and
   stored on the Supervisor (the `reviewAckTimeoutDur` field pattern), so the
   goroutine never touches viper directly. Interval floor (reconciled
   2026-11-09): `viper.GetDuration`/`cast.ToDuration` maps a unitless
   numeric (`PI_SUPERVISOR_JANITOR_INTERVAL=2`) to 2ns, which would spin the
   sweep into a hot loop; the supervisor resolves `<=0` to the registered
   default and clamps anything under one minute up to one minute
   (`janitorInterval`) BEFORE `time.NewTicker`.

2. **Eligibility — terminal states only.** A candidate is a job whose
   persisted state is one of the states from which no build round can ever
   resume: `done`, `fatal`, `review_done`, `review_exhausted` — the owner's
   "failure or success" set (the review terminals are in
   `events.Event.Terminal`'s set for the same reason). **`stopped` is never
   eligible**: a daemon-stopped job is the substrate of watch-driven resume
   (ADR-0017), and an operator stop means "stay stopped", not "delete me".
   Age must exceed `janitor_archive_after`, anchored on the state's
   end-of-run timestamp (§4.1). `running`/`reviewing` are ineligible by
   construction (they are not terminal) — the janitor never races a live
   round. Reconciliation note (2026-11-09): today no path persists
   `review_done`/`review_exhausted` onto a state file — a review campaign
   lands its terminal verdict as `done`/`fatal` and the EVENT carries the
   `review_*` name (that is what `events.Event.Terminal`'s synchrony is
   about). The two review verbs stay in the janitor's match set for parity
   with the event vocabulary, so a future path persisting them directly is
   already archiveable. One exclusion on top (reconciled 2026-11-09): a
   terminal job whose `State.ReviewBaseline` is still set is NOT a candidate
   — the baseline is the only channel a post-campaign re-check (explicit or
   push-triggered) reads to notice NEW threads on that PR; archiving would
   sever it. The job re-enters eligibility when that audience state is
   cleared, the same way it does after a campaign re-entry.

3. **Skip live or armed jobs.** Even a `done` job may be legally re-entered
   by an armed review campaign (ADR-0016: `review --pr N` + start). A
   candidate whose runner is `active` or holds an armed campaign
   (`r.review != nil`) is skipped this sweep; it becomes eligible when the
   campaign reaches its own terminal state, which re-stamps the end
   timestamp — the clock restarts, correctly.

4. **Judgment calls made here (owner-vetoable).**

   - **4.1 Age anchor = new `State.EndedAt` field, mtime fallback.** Every
     terminal transition stamps `State.EndedAt` (RFC3339) — in
     `runner.finish()` and in the loop's direct state writes. Grep for
     `state.State` assignments (reconciled 2026-11-09 with the shipped
     code): the terminal-writing sites are `finish()` (the fatal funnel,
     including the review-exhausted path, which persists `fatal`), the
     build done-gate's `closedJob` branch, the review gate's clean close
     (persisted as `done` while the `review_done` EVENT carries the
     verdict), and the CI-stall-cap fatal write. `Stop()` is deliberately
     NOT stamped (stopped is ineligible), and neither is `Shutdown`'s
     `stopped` write. For states persisted before this field existed, the
     janitor falls back to the state file's mtime, which for a terminal job
     is its last terminal write. The explicit field is preferred because the
     archive decision is machine-readable evidence, not filesystem inference.
     The anchor's stat runs OUTSIDE the supervisor's mutexes (reconciled
     2026-11-09, kody): the sweep captures the stamp as a string under the
     locks and resolves the mtime per candidate afterwards; losing the race
     on a stamp change costs one sweep, never a control-op stall.
   - **4.2 The session JSONL is archived together with the def.** The def
     moving to `_archive/` already makes the job unrecoverable by the daemon;
     quarantining its session through the existing `job.Quarantine`
     (`_archived-stale/`, move-only, timestamped, skipped by `FindSession`)
     also removes the transcript from every future adoption scan. A session
     that no longer exists is a no-op, as `Quarantine` already guarantees.

5. **Archive action per candidate — move-only, bytes preserved.** The
   whole sequence for ONE candidate runs inside one critical section
   holding `s.mu` then `r.mu` (reconciled 2026-11-09): the candidate is
   re-validated in the SAME critical section that performs the moves and
   drops the runner, so a `Start`/`Restart`/review re-entry either lands
   before the lock (and the re-validation skip fires) or waits and then
   finds an unknown job — the snapshot-to-move gap is closed, and the job
   cannot be archived under a live run (qodo; kody).

   - Session FIRST: `job.Quarantine(j.SessionPath)` per §4.2 (re-ordered
     reconciled 2026-11-09: quarantine before the def move so a quarantine
     failure strands nothing — the def stays in `jobs/` and the next sweep
     re-candidates the job identically).
   - Def: `jobs/<name>.json` → `jobs/_archive/<name>.json`. The `_archive`
     subdirectory is skipped by `LoadJobs`'s dir-only skip; restoring is
     `mv` back + `pi-supervisor reload`, unchanged from the manual
     convention. A name collision in `_archive/` (a job name re-used after
     an earlier archive) advances the collision suffix — `_<timestamp>`
     first, then a counter on same-second collision — via
     link-then-remove (`os.Link` fails `EEXIST` on ANY existing
     destination, including a dangling symlink, then `os.Remove`; a failed
     `Remove` after a successful `Link` leaves BOTH files and is reported:
     never an overwrite, never a silent byte loss; reconciled 2026-11-09,
     qodo).
   - State: `state/<name>.json` → `jobs/_archive/<name>.state.json` (same
     collision scheme; reconciled 2026-11-09, qodo/kody): a surviving state
     file made a re-used job name inherit the OLD terminal state — `done`
     plus the old `EndedAt` — and get archived (or refused by `Start` with
     `KindAlreadyDone`) before its next run could begin. This is the
     archived job's OWN file moving with it, distinct from the non-goal's
     orphaned-state sweep below: orphaned state whose def is long gone
     stays untouched.
   - Map sync: the janitor never hands the map to `LoadJobs` (reconciled
     2026-11-09, kody/qodo: a post-batch `LoadJobs()` dropped ANY runner
     whose def transiently failed to parse/validate — a live job could
     vanish from `status`/`stop` for an hour). Instead the successfully
     archived runner is deleted from `s.jobs` in the same critical section
     as the moves (`delete(s.jobs, name)`) — targeted removal only.
   - Per-candidate errors are logged and skip that candidate; the sweep
     continues. A quarantine failure strands nothing (the def never
     moves). A def that proves absent at move time (an earlier sweep
     tripped after moving it, or a hand `mv` mid-run) RECOVERS: the
     remaining state move + session quarantine + runner drop + event
     complete the archival instead of warn-looping every sweep, and the
     event honestly names the destination as `already-moved` (the archived
     def's collision-suffixed location is unknowable from here). A
     pathological state file that cannot move (e.g. a directory wedged
     into its name) fails loudly on every sweep until repaired — visible
     by design, never silent.

6. **Observability — one event per archived job** through the single
   `emit` funnel: `job_archived`, with `Info` carrying `state=<terminal
   state> def=<archive path|already-moved> session=<quarantine path|none>`,
   and the event envelope carrying the worktree/PR context explicitly (the
   runner is already out of the map when it emits — `emitPrepared`, the
   same journal+audit+broker path as `emitEnvelope`). It flows to the
   journal, the per-job audit JSONL, and the event broker.

   **It IS terminal** (reconciled 2026-11-09, superseding this ADR's own
   earlier "NOT terminal" text): when the archive happens the archived
   job's run is over and its future held nothing — a watcher left hanging
   on it is not a grace period, it is a deadlock. `events.Event.Terminal`
   and the `watch` CLI exit list name `job_archived` in the SAME commit
   (invariant 22's sync rule — the two lists moved together). A `watch`
   that CONNECTS after the archive is answered by the same terminal
   precheck: the control server consults `Supervisor.Archived`, so the
   relaxation looks past the unknown-job refusal ONLY for names whose def
   really lives in `_archive/` — never-known job names stay refused
   exactly as before. Restoring the job remains the manual convention:
   `mv` the def back into `jobs/` and reload.

## Consequences

- Terminal jobs vanish from `status`/`job list` 7 days (default) after they
  ended, and their transcripts stop being adoptable. Nothing is deleted —
  both layers keep every byte, movable back by hand.
- The new `State.EndedAt` key appears in every freshly written state file;
  old state files keep loading (JSON decode ignores nothing new — the field
  is `omitempty` and absent means "use mtime"). ADR-0024's strict key check
  applies to job DEFS, not state files, so no compat gate is touched.
- Operators regain a deterministic question: "why is this job gone?" is
  answered by `jobs/_archive/` and the `job_archived` audit line, greppable
  in `~/.pi/supervisor/events/<name>.jsonl` and the journal.
- Config change takes effect at daemon restart (or reload of the process
  state), consistent with the rest of the daemon's config.

## Non-goals

- No CLI subcommand for the janitor. Manual `mv` in/out of `_archive/` plus
  `reload` remains the restore path; a sweep is automatic or nothing.
- No deletion, compaction, or remote upload of anything — move-only, both
  layers, always.
- No sweep of orphaned state files (a `state/<name>.json` whose def is
  already gone is harmless dead weight and stays). The archived job's own
  state file is not an orphan — it moves to `_archive/<name>.state.json`
  with its def (§5).
- No `/tmp/pi_*` artifact cleanup. `/tmp` is ephemeral and external tooling
  greps those names (invariant 5).
- No per-job opt-out. The retention knob is global; a job that must survive
  longer is an operator concern (restore it, or raise the interval).
