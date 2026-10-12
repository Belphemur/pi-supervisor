# pi-supervisor Hermes plugin

Typed Hermes tools for the [pi-supervisor](../doc/adr/0022-hermes-plugin.md)
daemon, per **ADR-0022** (`doc/adr/0022-hermes-plugin.md`). Pure Python
stdlib — no `python_dependencies`.

## Why it exists

The prose skill told the model to arm `pi-supervisor watch <job> -t` as a
background terminal run with notify. Flash-tier models intermittently OMIT
trailing optional tool-call parameters (`notify`, sometimes `background`)
under reasoning load — the arguments JSON stays valid, so Hermes applies
defaults and the watch arms **silent**: the terminal `done`/`fatal` event
reaches nobody (2026-10-12 forensics). The plugin moves the flags from model
output into plugin code, and verifies every arm before reporting success.

## Install

`install/install.sh` syncs this directory to
`~/.hermes/plugins/pi-supervisor/` and gates on
`hermes plugins doctor pi-supervisor --ci` (a missing `hermes` on PATH
degrades to a warning, never failing the daemon install).

## Tools

| Tool | Contract |
|---|---|
| `pi_status(job?)` | `pi-supervisor status [job]`; single-job form returned parsed (the daemon's one JSON document) |
| `pi_steer(job, message, interrupt?, no_wait?)` | `pi-supervisor steer <job> [-i] [-n] <msg>`; surfaces the factual report (forwarded / held / no live round / not confirmed) |
| `pi_logs(job, lines=50)` | `pi-supervisor logs <job> <lines>` |
| `pi_start(job, reload?)` | `pi-supervisor start <job>`; `reload=true` first runs the daemon-wide `pi-supervisor reload` — the daemon loads job definitions ONLY on reload, so a just-written `jobs/<name>.json` is "unknown job" until then; a reload failure aborts BEFORE the start (`stage: "reload"`), never a stale-def start. A done job refuses with exit 1 unless a review campaign is armed (ADR-0016) |
| `pi_review(job, pr?, auto?, rounds?, type_?)` | `pi-supervisor review <job> [--pr N \| --auto] [--rounds N] [--type …]`; `--pr` and `--auto` mutually exclusive (validated in the plugin, exit-2 shape); an EXPLICIT `rounds: 0` is passed through as `--rounds 0` (the CLI's derive-from-open-threads budget) — only an omitted `rounds` takes the CLI default |
| `pi_ack(job, event_id)` | `pi-supervisor ack <job> --event <id>`; ack_id comes from the `bulk_resolve_requested` watch event (NOTE: the CLI takes the job positional — deviation from the ADR sketch, the CLI contract wins) |
| `pi_watch(job)` | **THE ARM** — dispatches `terminal(command="<cli> watch <job> -t", background=true, notify=true, notify_on_complete=true, persist_on_release=true)` and VERIFIES it (see below). There is no persistence opt-out: the arm always persists (invariant 6). `<cli>` is the resolved absolute path of the pi-supervisor binary (PATH, then the installer's `~/.local/bin`), so the arm works even when Hermes has no `~/.local/bin` on PATH |
| `pi_watch_probe(job, timeout_s=600)` | plain one-shot `watch` WITHOUT `-t` — exits on the first event; result marked `probe: true`; never the delivery arm |

Slash command: `/piwatch <job>` — the same verified arm for the human path.

## Exit-code mapping (ADR-0008)

Every CLI call is mapped: `0` → ok, `1` → `refused: true` with the daemon's
reason, `2` → `usage_error: true` (the CALL is wrong, not the daemon).
Refused is never success.

## The verify-the-arm rule

`pi_watch` never trusts the dispatch. Before returning success it requires,
from the `terminal` tool result:

1. `output` contains `Background process started`;
2. a `session_id` (process id) is present;
3. `notify_on_complete: true` (a `subagent_note` in the result — the
   delegated-child demotion — also fails the arm);
4. `persist_on_release: true` — the arm always sends it and the spawn echoes
   it only when it took; an unconfirmed echo means the watch would die with
   the arming session, the same dropped-parameter class, and fails the arm;
5. belt and braces: if the process already completed, its record at
   `~/.hermes/logs/process-results/<session_id>.json` must also carry
   `notify_on_complete: true`.

Anything else returns `{"armed": false, "action": "RE-ARM REQUIRED", …}` —
never a silent success. If `ctx.dispatch_tool` is missing or raises, the
plugin refuses with the manual-arm incantation
(`{"armed": false, "fallback": "manual-arm"}`) rather than degrading to an
unanchored spawn: a watch spawned without session anchoring has nowhere to
deliver — the ADR-0003 failure mode.

Delivery stays invariant-6-pure: the blocking watch client armed as a
background run is the ONLY delivery path. No cron, no webhook, no polling —
and none will be added. `releases_on` names exactly the non-terminal events
on which `watch -t` actually exits — `task_completed` and
`task_lookup_failed` (the ADR-0014 owner decision in the CLI's watch loop);
the LLM wakes, verifies the claim, and re-arms. Every OTHER non-terminal
event (round_done, backoff, ci_stall, …) is streamed but does NOT release
the process, so the result never promises a wake-up that cannot happen.

## Tests

```bash
cd hermes-plugin && python -m pytest test_plugin.py -q
```