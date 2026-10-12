"""pi-supervisor Hermes plugin — ADR-0022.

Every tool wraps the `pi-supervisor` cobra CLI (the ONLY daemon surface,
ADR-0008 exit codes 0/1/2) and returns a JSON string. No sockets, no state
files, no transcript tailing, no cron/webhook delivery (invariant 6).

The load-bearing tool is `pi_watch`: it arms `pi-supervisor watch <job> -t`
as a Hermes background terminal run THROUGH ctx.dispatch_tool, so the arm's
flags are CODE, never model output — the flash-tier dropped-parameter
failure (2026-10-12 forensics) cannot recur — and the arm is VERIFIED before
success is reported. A watch that arms without notify (or without the
session anchor) is refused loudly, never a silent success: that silent arm
was the failure that motivated this plugin.

Pure Python stdlib only.
"""

import json
import os
import re
import subprocess
from pathlib import Path

CLI = "pi-supervisor"

# ADR-0008 exit codes.
EXIT_OK = 0
EXIT_REFUSED = 1
EXIT_USAGE = 2

# Job names feed an argv (never a shell), but a leading "-" would be parsed
# as a flag by cobra; keep names to what job files actually look like.
_JOB_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")

# Where Hermes writes each background process's result record
# (tools/process_registry_results.py: get_hermes_home()/logs/process-results).
PROC_RESULTS_SUBPATH = Path("logs") / "process-results"

# Non-terminal events a -t watch releases on (ADR-0014: trust-but-verify —
# the LLM wakes, verifies the claim, re-arms). Terminal events (done/fatal/
# stopped/review_*) are the end of the line and are deliberately absent.
RELEASE_EVENTS = [
    "task_completed", "task_lookup_failed", "round_done", "backoff",
    "ci_stall", "instant_exit", "empty_turn", "report_requested",
    "report_resteer", "review_threads_appeared", "job_started",
]

# The manual-arm incantation, returned whenever the plugin cannot arm through
# dispatch_tool: a watch spawned WITHOUT session anchoring has nowhere to
# deliver the terminal event (ADR-0003 failure mode), so we refuse rather
# than degrade to an unanchored spawn.
MANUAL_ARM_INCATATION = (
    'terminal(command="pi-supervisor watch <job> -t", background=true, '
    'notify=true, persist_on_release=true)'
)

# watch probe (plain, no -t): exits on the FIRST event. A round can run for
# minutes before that, so the default is generous but bounded.
PROBE_TIMEOUT_S = 600

# Ordinary CLI verbs answer immediately; steer may wait out a between-rounds
# backoff (~95 s worst case, ADR-0005), so allow more.
CLI_TIMEOUT_S = 180


def _job(name):
    """Validate a job name, returning an error message or None."""
    if not name or not isinstance(name, str) or not _JOB_RE.match(name):
        return (
            f"invalid job name {name!r}: must match [A-Za-z0-9][A-Za-z0-9._-]*"
        )
    return None


def _proc_results_dir():
    """The Hermes process-results directory (overridable for tests)."""
    override = os.environ.get("PI_SUPERVISOR_PROC_RESULTS_DIR")
    if override:
        return Path(override)
    home = os.environ.get("HERMES_HOME") or str(Path.home() / ".hermes")
    return Path(home) / PROC_RESULTS_SUBPATH


def _dump(payload):
    return json.dumps(payload, ensure_ascii=False)


def run_cli(argv, timeout=CLI_TIMEOUT_S):
    """Run `pi-supervisor <argv>` and map the exit code (ADR-0008).

    Returns (payload_dict, exit_code). Exit 0 -> ok (stdout the payload);
    1 -> refused (daemon answered but said no); 2 -> usage error (the CALL
    is wrong, not the daemon). spawn failures and timeouts get exit -1 and
    ok=False.
    """
    try:
        proc = subprocess.run(
            [CLI] + argv, capture_output=True, text=True, timeout=timeout
        )
    except FileNotFoundError:
        return {"ok": False, "error": f"{CLI} binary not found on PATH"}, -1
    except subprocess.TimeoutExpired:
        return {
            "ok": False,
            "error": f"{CLI} {' '.join(argv)} timed out after {timeout}s",
        }, -1

    code = proc.returncode
    stdout = (proc.stdout or "").strip()
    stderr = (proc.stderr or "").strip()
    if code == EXIT_OK:
        return {"ok": True, "exit_code": 0, "output": stdout}, 0
    if code == EXIT_REFUSED:
        return {
            "ok": False,
            "exit_code": 1,
            "refused": True,
            "reason": stdout or stderr or "(daemon refused without a message)",
        }, 1
    if code == EXIT_USAGE:
        return {
            "ok": False,
            "exit_code": 2,
            "usage_error": True,
            "error": stderr or stdout or "(usage error without a message)",
        }, 2
    # Unknown code: not a documented contract — surface everything, fail.
    return {
        "ok": False,
        "exit_code": code,
        "error": stdout or stderr or f"exit {code}",
    }, code


# --------------------------------------------------------------------------
# Tool handlers. Hermes calls plugin tool handlers as handler(args, **kw);
# args is the model-supplied arguments dict. Every handler returns a string.
# --------------------------------------------------------------------------


def _pi_status(args, **_kw):
    """pi_status(job="") -> `pi-supervisor status [job]`.

    The single-job form prints exactly one JSON document (invariant 9) —
    return it parsed as `status`. The all-jobs form is a human table — pass
    the text through.
    """
    job = (args or {}).get("job") or ""
    if job:
        err = _job(job)
        if err:
            return _dump({"ok": False, "usage_error": True, "error": err})
        payload, _ = run_cli(["status", job])
    else:
        payload, _ = run_cli(["status"])
    if payload.get("ok") and job:
        try:
            payload["status"] = json.loads(payload.pop("output"))
        except ValueError:
            payload["ok"] = False
            payload["error"] = (
                "status stdout was not the promised single JSON document "
                "(ADR-0006 invariant): "
                + str(payload.pop("output", ""))[:400]
            )
    return _dump(payload)


def _pi_steer(args, **_kw):
    """pi_steer(job, message, interrupt=False, no_wait=False)."""
    a = args or {}
    job = a.get("job") or ""
    message = a.get("message") or ""
    err = _job(job)
    if err:
        return _dump({"ok": False, "usage_error": True, "error": err})
    if not message:
        return _dump({
            "ok": False, "usage_error": True,
            "error": "message is required (the steer text)",
        })
    argv = ["steer", job]
    if a.get("interrupt"):
        argv.append("-i")
    if a.get("no_wait"):
        argv.append("-n")
    argv.append(message)
    payload, _ = run_cli(argv)
    # steer prints a factual report (forwarded / held / no live round /
    # not confirmed); surface it verbatim as report lines.
    if payload.get("ok") or payload.get("refused"):
        lines = (payload.get("output") or payload.get("reason") or "")
        payload["report"] = [ln for ln in lines.splitlines() if ln.strip()]
    return _dump(payload)


def _pi_logs(args, **_kw):
    """pi_logs(job, lines=50)."""
    a = args or {}
    job = a.get("job") or ""
    err = _job(job)
    if err:
        return _dump({"ok": False, "usage_error": True, "error": err})
    try:
        lines = int(a.get("lines") or 50)
    except (TypeError, ValueError):
        return _dump({
            "ok": False, "usage_error": True, "error": "lines must be an int",
        })
    if lines <= 0:
        return _dump({
            "ok": False, "usage_error": True, "error": "lines must be > 0",
        })
    payload, _ = run_cli(["logs", job, str(lines)])
    return _dump(payload)


def _pi_review(args, **_kw):
    """pi_review(job, pr=0, auto=False, rounds=0, type_="").

    --pr and --auto are mutually exclusive (validated HERE, exit-2 shape —
    never a wasted round trip to the daemon).
    """
    a = args or {}
    job = a.get("job") or ""
    err = _job(job)
    if err:
        return _dump({"ok": False, "usage_error": True, "error": err})
    pr = a.get("pr") or 0
    auto = bool(a.get("auto"))
    rounds = a.get("rounds") or 0
    type_ = a.get("type_") or a.get("type") or ""
    try:
        pr = int(pr)
        rounds = int(rounds)
    except (TypeError, ValueError):
        return _dump({
            "ok": False, "usage_error": True, "error": "pr/rounds must be ints",
        })
    if pr and auto:
        return _dump({
            "ok": False, "usage_error": True,
            "error": "--pr and --auto are mutually exclusive: pass pr OR auto, not both",
        })
    if not pr and not auto:
        return _dump({
            "ok": False, "usage_error": True,
            "error": "one of pr (manual campaign) or auto (completion-gate trigger) is required",
        })
    argv = ["review", job]
    argv += ["--pr", str(pr)] if pr else ["--auto"]
    if rounds:
        argv += ["--rounds", str(rounds)]
    if type_:
        if type_ not in ("acceptance", "rebuttal"):
            return _dump({
                "ok": False, "usage_error": True,
                "error": f"type_ must be 'acceptance' or 'rebuttal', got {type_!r}",
            })
        argv += ["--type", type_]
    payload, _ = run_cli(argv)
    return _dump(payload)


def _pi_ack(args, **_kw):
    """pi_ack(job, event_id) -> `pi-supervisor ack <job> --event <id>`.

    The CLI takes a JOB positional (cobra ExactArgs(1)) plus the ack id from
    the bulk_resolve_requested event — the watch payload carries both.
    """
    a = args or {}
    job = a.get("job") or ""
    event = a.get("event_id") or a.get("event") or ""
    err = _job(job)
    if err:
        return _dump({"ok": False, "usage_error": True, "error": err})
    if not event:
        return _dump({
            "ok": False, "usage_error": True,
            "error": "event_id is required (the ack_id from bulk_resolve_requested)",
        })
    payload, _ = run_cli(["ack", job, "--event", event])
    return _dump(payload)


# --------------------------------------------------------------------------
# pi_watch — the load-bearing tool.
# --------------------------------------------------------------------------

def _verify_arm(dispatch_result, proc_results_dir=None):
    """Verify a dispatched `terminal` arm BEFORE calling it a success.

    dispatch_result is the raw string returned by ctx.dispatch_tool. The
    background spawn path (tools/terminal_tool_background.py) returns JSON
    with output="Background process started", session_id, and
    notify_on_complete=true when the notify arm actually took. Belt and
    braces: if the process already completed, its record at
    <HERMES_HOME>/logs/process-results/<session_id>.json must also carry
    notify_on_complete=true.

    Returns (armed: bool, info: dict). Never raises.
    """
    info = {}
    if not isinstance(dispatch_result, str) or not dispatch_result.strip():
        return False, {"why": "dispatch returned an empty result", "observed": dispatch_result}
    try:
        result = json.loads(dispatch_result)
    except ValueError:
        return False, {
            "why": "dispatch result was not JSON",
            "observed": dispatch_result[:400],
        }
    if not isinstance(result, dict):
        return False, {"why": "dispatch result was not a JSON object", "observed": str(result)[:400]}
    info["dispatch"] = {k: result.get(k) for k in
                        ("session_id", "pid", "exit_code", "error",
                         "notify_on_complete", "persist_on_release")}
    if "Background process started" not in str(result.get("output", "")):
        why = result.get("error") or str(result.get("output", ""))[:200] or "no spawn confirmation"
        return False, dict(info, why=f"arm did not start a background process: {why}")
    session_id = result.get("session_id") or result.get("id")
    if not session_id:
        return False, dict(info, why="spawn confirmed but no session/process id in the result")
    info["proc"] = session_id

    # Delegated-child demotion: the spawn strips notify_on_complete and says
    # so. Delivery would never reach this session — refuse, loudly.
    if result.get("subagent_note"):
        return False, dict(info, why=(
            "dispatch ran in a delegated-child context: notify_on_complete was "
            "demoted (" + str(result.get("subagent_note"))[:300] + ") — arm from "
            "the parent session instead"
        ))
    if result.get("notify_on_complete") is not True:
        return False, dict(info, why=(
            "armed WITHOUT notify_on_complete (the dropped-parameter failure "
            "this plugin exists to prevent) — RE-ARM REQUIRED"
        ))
    if result.get("persist_on_release") is not True:
        info["persist_warning"] = (
            "persist_on_release not confirmed in the dispatch result; the watch "
            "dies with the arming session"
        )

    # Belt and braces: a fast-exiting process already has its record on disk.
    results_dir = Path(proc_results_dir) if proc_results_dir else _proc_results_dir()
    record_path = results_dir / f"{session_id}.json"
    if record_path.exists():
        try:
            record = json.loads(record_path.read_text())
        except (OSError, ValueError) as exc:
            return False, dict(info, why=f"proc record {record_path} unreadable: {exc}")
        if record.get("notify_on_complete") is not True:
            return False, dict(info, why=(
                f"proc record {record_path.name} shows notify_on_complete="
                f"{record.get('notify_on_complete')!r} — RE-ARM REQUIRED"
            ))
        info["record_verified"] = True
    info["notify_verified"] = True
    return True, info


def arm_watch(ctx, job, persist=True, proc_results_dir=None):
    """The verified arm, shared by the pi_watch tool and /piwatch.

    Flags are CODE (dispatch args), never model output. On any dispatch
    failure the return is a loud refusal with the manual-arm incantation —
    never a degraded unanchored spawn.
    """
    err = _job(job)
    if err:
        return _dump({"ok": False, "usage_error": True, "armed": False, "error": err})
    dispatch = getattr(ctx, "dispatch_tool", None)
    if not callable(dispatch):
        return _dump({
            "ok": False, "armed": False, "fallback": "manual-arm",
            "error": "ctx has no dispatch_tool — the plugin cannot anchor the arm",
            "manual_arm": MANUAL_ARM_INCATATION,
        })
    arm = {
        "command": f"pi-supervisor watch {job} -t",
        "background": True,
        "notify": True,
        "notify_on_complete": True,   # legacy alias; explicit notify wins
        "persist_on_release": bool(persist),
    }
    try:
        raw = dispatch("terminal", arm)
    except Exception as exc:  # noqa: BLE001 — fail loud, never degrade
        return _dump({
            "ok": False, "armed": False, "fallback": "manual-arm",
            "error": f"dispatch_tool raised: {exc}",
            "manual_arm": MANUAL_ARM_INCATATION,
        })
    armed, info = _verify_arm(raw, proc_results_dir)
    if not armed:
        out = {
            "ok": False, "armed": False, "job": job,
            "action": "RE-ARM REQUIRED", **info,
        }
        if "fallback" not in out:
            out["manual_arm"] = MANUAL_ARM_INCATATION
        return _dump(out)
    return _dump({
        "ok": True, "armed": True, "job": job,
        "notify_verified": True,
        "proc": info.get("proc"),
        "persist_on_release": bool(persist),
        "releases_on": RELEASE_EVENTS,
        "note": ("verify task claims on release; re-arm until a TERMINAL event "
                 "(done/fatal/stopped/review_done/review_exhausted)"),
        **{k: v for k, v in info.items()
           if k in ("persist_warning", "record_verified")},
    })


def _pi_watch(args, **_kw):
    """pi_watch(job, persist=True) — arm the -t watch, verify, report."""
    a = args or {}
    return arm_watch(_WATCH_CTX[0], a.get("job") or "",
                     persist=bool(a.get("persist", True)))


def _pi_watch_probe(args, **_kw):
    """pi_watch_probe(job, timeout_s=600) — plain `watch` WITHOUT -t.

    A one-shot probe: exits after the FIRST event. Marked probe=true so it
    can never be confused with the armed delivery path.
    """
    a = args or {}
    job = a.get("job") or ""
    err = _job(job)
    if err:
        return _dump({"ok": False, "usage_error": True, "probe": True, "error": err})
    try:
        timeout = int(a.get("timeout_s") or PROBE_TIMEOUT_S)
    except (TypeError, ValueError):
        timeout = PROBE_TIMEOUT_S
    payload, code = run_cli(["watch", job], timeout=timeout)
    payload["probe"] = True
    if code == -1 and "timed out" in str(payload.get("error", "")):
        payload["note"] = (
            "probe expired with no event — the round is likely still running; "
            "this is NOT an arm (never -t)"
        )
    return _dump(payload)


# ctx captured at register() time so handlers can reach it (dispatch_tool
# carries the parent agent's session anchoring).
_WATCH_CTX = [None]


def _piwatch_command(raw_args, **_kw):
    """/piwatch <job> — the same verified arm, human path."""
    job = (raw_args or "").strip().split()
    if not job:
        return _dump({
            "ok": False, "usage_error": True, "armed": False,
            "error": "usage: /piwatch <job>",
        })
    return arm_watch(_WATCH_CTX[0], job[0])


# --------------------------------------------------------------------------
# register
# --------------------------------------------------------------------------

_TOOL_SCHEMA_BASE = {"type": "object"}

# The toolset namespace this plugin registers under.
TOOLSET = "pi_supervisor"


def register(ctx):
    """Register the pi-supervisor tools and the /piwatch slash command."""
    _WATCH_CTX[0] = ctx

    ctx.register_tool(
        name="pi_status",
        toolset=TOOLSET,
        schema={
            **_TOOL_SCHEMA_BASE,
            "description": (
                "Query the pi-supervisor daemon: pass job for one job (returns "
                "the daemon's JSON status document: state, round, session, "
                "pr_url, tasks), omit job for the all-jobs table."
            ),
            "properties": {"job": {"type": "string", "description": "job name (optional)"}},
            "required": [],
        },
        handler=_pi_status,
    )
    ctx.register_tool(
        name="pi_steer",
        toolset=TOOLSET,
        schema={
            **_TOOL_SCHEMA_BASE,
            "description": (
                "Steer a running pi round: forwards a prompt frame to the live "
                "session (waits for pi's ack). interrupt=true SIGINTs pi first "
                "so the current turn stops and this steer is next; no_wait=true "
                "skips the ack wait. Refuses with exit 1 reasons like "
                "no-live-round / not-confirmed."
            ),
            "properties": {
                "job": {"type": "string"},
                "message": {"type": "string"},
                "interrupt": {"type": "boolean", "description": "SIGINT pi's group first (ADR-0007)"},
                "no_wait": {"type": "boolean", "description": "return once the frame is on disk"},
            },
            "required": ["job", "message"],
        },
        handler=_pi_steer,
    )
    ctx.register_tool(
        name="pi_logs",
        toolset=TOOLSET,
        schema={
            **_TOOL_SCHEMA_BASE,
            "description": "Tail a job's run log (last N lines, default 50).",
            "properties": {
                "job": {"type": "string"},
                "lines": {"type": "integer", "description": "tail length (default 50)"},
            },
            "required": ["job"],
        },
        handler=_pi_logs,
    )
    ctx.register_tool(
        name="pi_review",
        toolset=TOOLSET,
        schema={
            **_TOOL_SCHEMA_BASE,
            "description": (
                "Arm a daemon-orchestrated review campaign on a job's PR: "
                "pr=N for a manual campaign OR auto=true for the "
                "completion-gate trigger (mutually exclusive). rounds=budget, "
                "type_='acceptance'|'rebuttal'."
            ),
            "properties": {
                "job": {"type": "string"},
                "pr": {"type": "integer", "description": "PR number for a manual campaign"},
                "auto": {"type": "boolean", "description": "arm the completion-gate trigger instead"},
                "rounds": {"type": "integer", "description": "campaign round budget (0 = derive)"},
                "type_": {"type": "string", "enum": ["acceptance", "rebuttal"]},
            },
            "required": ["job"],
        },
        handler=_pi_review,
    )
    ctx.register_tool(
        name="pi_ack",
        toolset=TOOLSET,
        schema={
            **_TOOL_SCHEMA_BASE,
            "description": (
                "Apply a pending bulk_resolve the daemon requested: pass the "
                "job and the event_id (ack_id) from the bulk_resolve_requested "
                "watch event. Unacked requests expire and threads stay open."
            ),
            "properties": {
                "job": {"type": "string"},
                "event_id": {"type": "string", "description": "ack id from bulk_resolve_requested"},
            },
            "required": ["job", "event_id"],
        },
        handler=_pi_ack,
    )
    ctx.register_tool(
        name="pi_watch",
        toolset=TOOLSET,
        schema={
            **_TOOL_SCHEMA_BASE,
            "description": (
                "THE ARM: launch `pi-supervisor watch <job> -t` as a verified "
                "Hermes background run with notify. Flags are set in code and "
                "the arm is verified (notify_on_complete=true) before success "
                "is reported. Releases on non-terminal events — verify task "
                "claims, then re-arm — and holds until a terminal event."
            ),
            "properties": {
                "job": {"type": "string"},
                "persist": {"type": "boolean", "description": "persist_on_release (default true)"},
            },
            "required": ["job"],
        },
        handler=_pi_watch,
    )
    ctx.register_tool(
        name="pi_watch_probe",
        toolset=TOOLSET,
        schema={
            **_TOOL_SCHEMA_BASE,
            "description": (
                "PROBE ONLY: a plain one-shot `pi-supervisor watch <job>` "
                "without -t — exits after the FIRST event. NOT the delivery "
                "arm; use pi_watch for that."
            ),
            "properties": {
                "job": {"type": "string"},
                "timeout_s": {"type": "integer", "description": "probe wait cap (default 600)"},
            },
            "required": ["job"],
        },
        handler=_pi_watch_probe,
    )

    ctx.register_command(
        "piwatch",
        handler=_piwatch_command,
        description="Arm a verified pi-supervisor watch on a job (same as pi_watch)",
        args_hint="<job>",
    )