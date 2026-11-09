# ADR-0025: Janitor — automatic archival of terminal jobs

## Status

Accepted (2026-11-09). Directed by the owner in chat: sessions (jobs) that
ended in failure or success must be archived automatically so they are never
reloaded, sweeping at daemon start and then every hour, the interval
configurable with 1h as the default, implemented as a daemon goroutine.
Two judgment calls this ADR adds beyond that directive are flagged in §4 and
can be vetoed before the implementation lands.

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
   | `janitor_interval` | `1h` | `viper.GetDuration` sweep cadence |
   | `janitor_archive_after` | `168h` | terminal age before archival |

   Defaults are registered with `viper.SetDefault` in `initConfig`; the
   values are read in `runDaemon` the way `socketPath()` reads `socket` and
   stored on the Supervisor (the `reviewAckTimeoutDur` field pattern), so the
   goroutine never touches viper directly.

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
   round.

3. **Skip live or armed jobs.** Even a `done` job may be legally re-entered
   by an armed review campaign (ADR-0016: `review --pr N` + start). A
   candidate whose runner is `active` or holds an armed campaign
   (`r.review != nil`) is skipped this sweep; it becomes eligible when the
   campaign reaches its own terminal state, which re-stamps the end
   timestamp — the clock restarts, correctly.

4. **Judgment calls made here (owner-vetoable).**

   - **4.1 Age anchor = new `State.EndedAt` field, mtime fallback.** Every
     terminal transition stamps `State.EndedAt` (RFC3339) in
     `runner.finish()` and in the done-gate branch that closes the job
     without `finish` — the two places terminal state is written while the
     loop is the actor. `Stop()` is deliberately NOT stamped (stopped is
     ineligible). For states persisted before this field existed, the
     janitor falls back to the state file's mtime, which for a terminal job
     is its last terminal write. The explicit field is preferred because the
     archive decision is machine-readable evidence, not filesystem inference.
   - **4.2 The session JSONL is archived together with the def.** The def
     moving to `_archive/` already makes the job unrecoverable by the daemon;
     quarantining its session through the existing `job.Quarantine`
     (`_archived-stale/`, move-only, timestamped, skipped by `FindSession`)
     also removes the transcript from every future adoption scan. A session
     that no longer exists is a no-op, as `Quarantine` already guarantees.

5. **Archive action per candidate — move-only, bytes preserved:**

   - Def: `jobs/<name>.json` → `jobs/_archive/<name>.json`. The `_archive`
     subdirectory is skipped by `LoadJobs`'s dir-only skip; restoring is
     `mv` back + `pi-supervisor reload`, unchanged from the manual
     convention. A name collision in `_archive/` (a job name re-used after
     an earlier archive) gains the same `_<timestamp>` suffix
     `job.Quarantine` uses, plus a counter guard on same-second collision —
     never an overwrite.
   - Session: `job.Quarantine(j.SessionPath)` per §4.2.
   - Map sync: after the batch, one `s.LoadJobs()` call reuses the tested
     reload path to drop the archived runners from the in-memory map — the
     janitor does not hand-mutate `s.jobs`.
   - Per-candidate errors (rename failure, missing dir) are logged and skip
     that candidate; the sweep continues. An archival that fails halfway
     (def moved, quarantine failed) leaves the job on disk in a consistent
     state: def archived, session still in place, map still holding the
     runner — retried by the next sweep (the def is gone from `jobs/`, so
     the candidate simply no longer parses out of the dir; the session
     quarantine is then inert). No partial-state cleanup logic is added.

6. **Observability — one event per archived job** through the single
   `emit` funnel: `job_archived`, with `Info` carrying `state=<terminal
   state> def=<archive path> session=<quarantine path|none>`. It flows to
   the journal, the per-job audit JSONL, and the event broker. **It is NOT
   terminal**: nothing is added to `events.Event.Terminal` or the watch
   exit list (invariant 22's sync rule). A watcher on a job being archived
   hears the event and keeps streaming; the job it watched simply ceases to
   exist in `status` afterward.

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
  already gone is harmless dead weight and stays).
- No `/tmp/pi_*` artifact cleanup. `/tmp` is ephemeral and external tooling
  greps those names (invariant 5).
- No per-job opt-out. The retention knob is global; a job that must survive
  longer is an operator concern (restore it, or raise the interval).
