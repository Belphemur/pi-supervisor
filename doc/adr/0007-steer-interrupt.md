# ADR-0007 — steer --interrupt (drop the running turn, then deliver)

Status: accepted (2026-10-02)

## Context

`pi-supervisor steer <job> "text"` (ADR-0005) writes a prompt frame to the
round's ctrl file. The client picks it up on its next poll and forwards it to
pi's stdin. That is a **queued** message: if the agent is mid-turn — a long
tool call, a `go test` run, a CI wait — the steer is answered only after that
turn finishes. An operator watching a campaign go off the rails wants the
current turn *dropped* and the steer picked up immediately; today there is no
way to express that.

Two mechanisms already exist and must not be reinvented:

- **process groups.** The client spawns pi with `SysProcAttr.Setpgid`, and
  every stop/kill path signals `-pid` (AGENTS.md invariant 3). So the daemon
  already knows how to reach the whole pi group.
- **the abort-drain handshake.** When the client sends an `abort` frame it
  holds further control frames until the aborted turn's `agent_end`, then
  releases them. A steer that lands mid-abort is therefore already delivered
  *after* the turn winds down, reported as held-then-forwarded.

## Decision

Add `steer --interrupt` (`-i`), which asks the daemon to **SIGINT pi's process
group before writing the frame**:

1. The daemon resolves the runner's current pid under `r.mu`.
2. `interruptPID(pid)` sends `syscall.Kill(-pid, syscall.SIGINT)` — the whole
   group, never the pid alone. SIGINT (not SIGTERM) so pi can abort the
   current generation and stay available for the steer.
3. Only then is the frame appended to the ctrl file, where the existing
   ack-wait reports the real outcome.

`Request.Interrupt` is the wire flag; `SteerReport.Interrupted` reports that
the signal was **sent**.

### The signal is requested, not obeyed

`Interrupted: true` means "the SIGINT was delivered to the group". Whether pi
stops its current generation is pi's business. The report therefore never
claims the turn was abandoned — only that it was asked to be. The
acknowledged outcome (`forwarded` / `held-then-forwarded`) remains the source
of truth for delivery, exactly as in ADR-0005.

### A failed signal writes nothing

If `interruptPID` fails (no live pid, dead group), `Steer` returns an error
and **does not write the frame**. Degrading silently to a plain queued steer
would produce the one outcome ADR-0005 exists to prevent: a steer the operator
reads as an interrupt that never happened. The detail names the failure and
the fact that no frame was written.

### No sleep in the daemon

The daemon does not sleep between the signal and the frame write. The client
already owns the drain handshake: a frame that lands mid-abort is held and
released when the aborted turn ends. Sleeping would only delay delivery the
client is already scheduling correctly.

## Consequences

- `steer -i` is the operator's "stop what you're doing and do this instead".
  Without the flag, steer keeps its queued semantics (a plain steer never
  signals — asserted by test).
- The interrupt adds no new dependency on the client; it reuses the process
  group and the drain handshake. A steer that lands mid-abort reports
  `held-then-forwarded`, which is the truthful outcome.
- Because SIGINT is best-effort, the daemon cannot prove the turn stopped. The
  ADR-0005 rule stands: the report shows what was sent and what pi
  acknowledged, and never more.

## Alternatives rejected

- **SIGTERM instead of SIGINT** — SIGTERM would tear pi down and end the
  round; there would be no session left to deliver the steer to.
- **Signal the pi pid alone** — violates process-group invariant 3 and misses
  anything pi spawned (its own tool subprocesses).
- **Sleep after the signal** — races the client's drain handshake and adds
  latency for no correctness gain.
- **Fall back to a queued steer when the signal fails** — produces a steer
  that looks like an interrupt but is not (see "failed signal writes nothing").