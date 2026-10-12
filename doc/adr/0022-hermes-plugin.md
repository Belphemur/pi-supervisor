# ADR-0022: Hermes plugin — typed tools and a server-side watch arm

Date: 2026-10-12

## Status

Accepted. Implementation follows this ADR.

## Context

Today the only Hermes integration is the `pi-supervisor` **skill**: prose that
instructs whatever model reads it to shell out to the cobra CLI and to arm
`pi-supervisor watch <job> -t` as a **background terminal run with notify**
(ADR-0003). Both halves now have a proven failure that prose cannot fix.

**The watch arm fails silently, and the failure is in the model, not the
tooling.** On 2026-10-12 a watch on `mealime-philosophy-fix` was armed with
the correct incantation, caught a `backoff`, released on the first
`task_completed` — and the completion notification never fired. Forensics
(`~/.hermes/logs/process-results/<proc>.json`) showed the background run had
`notify_on_complete: false`. The cause, reproduced twice independently:

1. **Direct OpenRouter repro** of the arming tool call against
   `z-ai/glm-5.3-flash`: with ~0 reasoning tokens the tool-call arguments
   carry all parameters (`background`, `command`, `notify`); with the same
   prompt under reasoning load (117+ reasoning tokens) the arguments arrive
   as valid JSON **missing `notify`** — `finish_reason: tool_calls`,
   well-formed JSON, no error anywhere. The model omits trailing optional
   parameters; the executor applies defaults.
2. **Session journals** (`~/.hermes/webui/sessions/_run_journal/…`) record
   dispatched `terminal` args of `{command}` only — even while multi-KB
   `command` strings arrive intact. The heavier the turn's reasoning, the
   more trailing parameters vanish (within one session: two parameters
   survived early, only one later).

A flash-tier model drops trailing optional fields under reasoning load. The
arm call is exactly the shape that loses them: three boolean flags behind a
long `command`. When `notify` is dropped, the watch arms **silent** — the
terminal `done`/`fatal`/`review_*` event reaches nobody. When `background`
is dropped, the call runs foreground and dies at the 600 s cap mid-wait.
Either way the skill's prose was followed correctly by a model that then
failed to serialize it. Restarting Hermes services does not fix it (tried
live); the parameter loss is in the model's emission, not in stale code.

The same fallibility taxes every other verb: `status`, `steer`, `ack`,
`review` are cobra invocations the model must remember flags for, with the
skill as the only memory.

## Decision

1. **A Hermes plugin lives in-repo at `hermes-plugin/`**, installed by
   `install/install.sh` into `~/.hermes/plugins/pi-supervisor/` alongside the
   daemon and the skill. The plugin is this product's own integration
   surface — same standing as `doc/skill/`, shipped and versioned with the
   daemon it drives. (Owner decision 2026-10-12: in-repo, not a standalone
   repo.)
2. **Query and control tools wrap the CLI, not the socket.** The cobra CLI is
   the stable, versioned interface with a documented exit-code contract
   (ADR-0008: 0 success / 1 refused / 2 usage). The plugin shells out and
   maps the exit codes; it never duplicates `internal/control` wire shapes in
   Python. (Owner decision 2026-10-12.) Tools: `pi_status`, `pi_steer`,
   `pi_ack`, `pi_review`, `pi_logs`, `pi_start` (with the daemon-only-loads-
   defs-on-reload rule folded in as a `reload` flag whose failure aborts
   before the start) — typed parameters, JSON results.
3. **`pi_watch(job)` performs the arm server-side.** The handler invokes
   Hermes' documented `ctx.dispatch_tool("terminal", …)` — the plugin-guide
   contract: "call any other tool with the parent agent's context
   (approvals, credentials, task_id) wired up automatically" — with the arm
   as **code, not model output**:

   ```python
   ctx.dispatch_tool("terminal", {
       "command": f"pi-supervisor watch {job} -t",
       "background": True,
       "notify": True,              # explicit notify wins on conflict
       "notify_on_complete": True,  # legacy alias passed too, guards executor skew
       "persist_on_release": True,  # invariant 6: the arm ALWAYS persists
   })
   ```

   The model supplies exactly one argument: `job`. The flags cannot be
   dropped because they are not emitted by the model. `task_id` inheritance
   keeps ADR-0003's delivery guarantee: the completion notification lands in
   the arming session, structurally.
4. **The handler verifies the arm before returning, and fails loud.** It
   checks the tool result for `Background process started`, resolves the
   process id, and reads
   `~/.hermes/logs/process-results/<proc>.json` to confirm
   `notify_on_complete: true`. Success returns a compact JSON
   (`armed`, `notify_verified`, `proc`). Any mismatch returns a machine
   failure naming the re-arm action — never a quiet "ok".
5. **`pi_watch_probe(job)` keeps the probe/arm distinction.** A plain
   one-shot `watch` (no `-t`) is a legitimate probe ("what happens next")
   and must stay available — but as a *separate* tool, so the model cannot
   confuse the two (the skill's rule, now encoded in the surface).
6. **`/piwatch <job>`** registers the same verified arm as a slash command
   for the human path.
7. **Trust-but-verify stays with the model.** `task_completed` and other
   non-terminal events release the watch on purpose (ADR-0014): the LLM
   wakes, verifies the claim, decides, and re-arms. The plugin makes each
   re-arm a one-argument call — cheap enough that a weak model does it — but
   the verification *judgment* is not automated away.

## Consequences

- Arm correctness no longer depends on model fidelity. The plugin works on
  flash-tier models; the diagnosed failure class is structurally removed.
- **Nested dispatch is proven live** (2026-10-12, first install): a one-shot
  Hermes session called `pi_watch(job="hermes-plugin")`; the handler's
  `ctx.dispatch_tool("terminal", …)` armed the run, and the verification read
  `notify_on_complete: true` from the proc JSON — `{"armed": true,
  "notify_verified": true, "persist_on_release": true}`. Note for testers:
  delegate-task children run a RESTRICTED toolset without plugin tools — the
  live test must be a real session (`hermes chat -q` or a gateway session),
  not a delegation child. Fallback path (manual-arm refusal on missing
  `dispatch_tool`) is implemented and unit-tested.
- install.sh gains a plugin step: sync `hermes-plugin/` to
  `~/.hermes/plugins/pi-supervisor/` (idempotent), then
  `hermes plugins doctor pi-supervisor` gates the install like the other
  verification steps.
- The plugin adds no daemon code; the CLI (ADR-0008) remains the only
  control surface it touches.

## Non-goals

- **No cron, webhook, or polling delivery variant** — invariant 6 and
  ADR-0003 stand: the blocking watch client armed as a background run is the
  only delivery path. The plugin must never grow a `pi_watch_cron`.
- **No MCP server.** An in-process plugin is lighter than a per-session
  stdio server; revisit only if non-Hermes MCP clients need the daemon.
- **No direct state-file reads.** `~/.pi/supervisor/state/` stays
  daemon-owned; the plugin learns state only through `status`.
- **No automated merge/ack on the human's behalf.** The ack gate
  (ADR-0012 §2.3) semantics are unchanged — `pi_ack` forwards an existing
  ack_id, it never invents consent.
