# ADR-0017: Watch-driven resume across daemon restarts

Date: 2026-10-07
Status: Accepted

## Context

`install.sh` restarts the systemd unit on every install, and operators bounce
the daemon for any binary update. Until now that restart DESTROYED every live
campaign: `Supervisor.Shutdown` killed the client process groups and the round
loop classified the exit as `stopped` — a terminal state, indistinguishable
from an explicit `pi-supervisor stop`. A watch armed across the restart then
received `run already stopped — nothing to wait for` on reconnect: technically
truthful, operationally wrong. Nobody asked the campaign to end; the daemon
was merely going away. Each install silently cost one round of progress and
one manual `pi-supervisor start` per job (observed on `mealime-search` /
`mealime-changelog`, 2026-10-06).

## Decision

**A daemon-shutdown stop is not an operator stop.** The two are recorded
distinctly and behave differently:

1. `job.State.StopSource` (`stop_source` in the state file) records who
   ordered the last stop: `operator` (`Supervisor.Stop`) or `daemon`
   (`Supervisor.Shutdown`). It is set BEFORE the stop channel closes, so a
   classification point can never observe a closed channel with an unset
   source, and it is cleared on the next `Start`.
2. The watch precheck (`Supervisor.Watch`) RESUMES a `stopped` job whose
   `StopSource == "daemon"`: a watcher re-arming is the proof that someone
   still cares about the campaign. An `operator` stop stays stopped — an
   explicit halt is intent to stop. `AlreadyRunning` (a racing second
   watcher) proceeds as a normal running-watch; any other refusal surfaces
   as a fail-loud `fatal` precheck.
3. The resume is the SAME `Start` path: pinned session, RESUME not LAUNCH
   (invariant 1), round counter intact (persisted at round start). No new
   launch machinery, no adoption protocol.

### Considered and rejected

- **Daemon auto-resumes everything on startup (Design A).** Resume would
  follow the daemon, not intent: every install/boot would silently restart
  every old campaign with no listener attached — unwatched API spend and
  boot surprises. The watch client is the ONLY delivery path (invariant 6);
  auto-resume decouples work from its witness.
- **The daemon never stops jobs (Design C: adopt running sessions).** pi's
  RPC session is a live pipe to the daemon process. Without a reader, pi
  wedges on a full pipe buffer; a new daemon cannot re-attach to another
  process's pipe. Mid-round session handoff is an upstream pi protocol
  feature, out of scope. Transcript replay could recover observations, but
  rc classification, get_state identity and steer timing are client-lifetime
  bound.

### Known limitation (accepted)

The watch client is the resume trigger. If the watcher dies permanently
during the restart window, the daemon-stopped job stays stopped — visible in
`status` (`stop_source: daemon`), resumable with one `pi-supervisor start` or
the next `watch`. That failure is loud and cheap, unlike Design A's silent
token burn.

## Consequences

- `systemctl --user restart pi-supervisor` (and every `install.sh` run) no
  longer ends campaigns that have an armed watch: the client wakes on the
  `stopped` event, re-arms (existing wake-and-rearm contract), and the re-arm
  resumes the session. One wake per install instead of one dead campaign.
- A deliberate halt is now unambiguous: `pi-supervisor stop <job>` — and
  ONLY that — parks a job for good.
- `stop_source` rides in the state file; old state files without the field
  default to `""`, which never resumes (fail-safe direction).
- The `stopped` event's `info` string is the client-facing distinction;
  the watch footer renders it verbatim and must never hardcode "operator".