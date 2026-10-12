"""Tests for the pi-supervisor Hermes plugin (ADR-0022).

The verification logic is the point of the plugin; it is tested hard:
exit-code mapping (stubbed subprocess), and pi_watch's arm verification
against a FAKE ctx with a canned dispatch result and canned proc records
on disk (tmp_path).
"""

import json
import subprocess
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parent))
import plugin  # noqa: E402


# --------------------------------------------------------------------------
# helpers
# --------------------------------------------------------------------------

class FakeCtx:
    """A ctx whose dispatch_tool returns a canned string (or raises)."""

    def __init__(self, result="", raises=None, with_dispatch=True):
        self.result = result
        self.raises = raises
        self.with_dispatch = with_dispatch
        self.dispatched = None
        if with_dispatch:
            self.dispatch_tool = self._dispatch

    def _dispatch(self, name, args):
        self.dispatched = (name, args)
        if self.raises:
            raise self.raises
        return self.result


def good_dispatch_result(proc="proc_abc123def456", **extra):
    """What Hermes' terminal background spawn actually returns on success."""
    result = {
        "output": "Background process started",
        "session_id": proc,
        "pid": 4242,
        "exit_code": 0,
        "error": None,
        "notify_on_complete": True,
        "persist_on_release": True,
    }
    result.update(extra)
    return json.dumps(result)


def write_proc_record(results_dir, proc, **fields):
    record = {
        "id": proc,
        "command": "pi-supervisor watch j -t",
        "notify_on_complete": True,
        "exit_code": 0,
    }
    record.update(fields)
    path = results_dir / f"{proc}.json"
    path.write_text(json.dumps(record))
    return path


@pytest.fixture
def results_dir(tmp_path, monkeypatch):
    monkeypatch.setenv("PI_SUPERVISOR_PROC_RESULTS_DIR", str(tmp_path))
    return tmp_path


def run_stub(monkeypatch, code, stdout="", stderr=""):
    """Stub plugin.subprocess.run to return a fixed exit code."""

    def fake_run(argv, **_kw):
        return subprocess.CompletedProcess(argv, code, stdout, stderr)

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)


# --------------------------------------------------------------------------
# exit-code mapping (ADR-0008)
# --------------------------------------------------------------------------

def test_exit_zero_is_ok(results_dir, monkeypatch):
    run_stub(monkeypatch, 0, stdout='{"name":"j","state":"running"}')
    payload = json.loads(plugin._pi_status({"job": "j"}))
    assert payload["ok"] is True
    assert payload["exit_code"] == 0
    assert payload["status"] == {"name": "j", "state": "running"}


def test_exit_one_is_refused_not_ok(results_dir, monkeypatch):
    run_stub(monkeypatch, 1, stdout="no live round")
    payload = json.loads(plugin._pi_steer({"job": "j", "message": "x"}))
    assert payload["ok"] is False
    assert payload["refused"] is True
    assert payload["exit_code"] == 1
    assert "no live round" in payload["reason"]
    assert payload["report"] == ["no live round"]


def test_exit_two_is_usage_error(results_dir, monkeypatch):
    run_stub(monkeypatch, 2, stderr="unknown flag: --nope")
    payload = json.loads(plugin._pi_steer({"job": "j", "message": "x"}))
    assert payload["ok"] is False
    assert payload["usage_error"] is True
    assert payload["exit_code"] == 2
    assert "--nope" in payload["error"]


def test_binary_missing_is_not_ok(results_dir, monkeypatch):
    def fake_run(argv, **_kw):
        raise FileNotFoundError("pi-supervisor")

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)
    payload = json.loads(plugin._pi_logs({"job": "j"}))
    assert payload["ok"] is False


def test_unknown_exit_code_fails_loud(results_dir, monkeypatch):
    run_stub(monkeypatch, 3, stderr="boom")
    payload = json.loads(plugin._pi_logs({"job": "j"}))
    assert payload["ok"] is False
    assert payload["exit_code"] == 3


# --------------------------------------------------------------------------
# per-tool argument handling
# --------------------------------------------------------------------------

def test_status_single_job_runs_status_job(results_dir, monkeypatch):
    seen = {}
    run_stub(monkeypatch, 0, stdout='{"state":"done"}')

    def fake_run(argv, **kw):
        seen["argv"] = argv
        return subprocess.CompletedProcess(argv, 0, '{"state":"done"}', "")

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)
    payload = json.loads(plugin._pi_status({"job": "j"}))
    assert seen["argv"] == ["pi-supervisor", "status", "j"]
    assert payload["status"] == {"state": "done"}


def test_status_all_jobs_passes_text_through(results_dir, monkeypatch):
    def fake_run(argv, **_kw):
        assert argv == ["pi-supervisor", "status"]
        return subprocess.CompletedProcess(argv, 0, "JOBS table...", "")

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)
    payload = json.loads(plugin._pi_status({}))
    assert payload["ok"] is True
    assert payload["output"] == "JOBS table..."


def test_status_non_json_stdout_is_an_error(results_dir, monkeypatch):
    run_stub(monkeypatch, 0, stdout="not json")
    payload = json.loads(plugin._pi_status({"job": "j"}))
    assert payload["ok"] is False
    assert "single JSON document" in payload["error"]


def test_steer_flag_order_and_message(results_dir, monkeypatch):
    seen = {}

    def fake_run(argv, **_kw):
        seen["argv"] = argv
        return subprocess.CompletedProcess(argv, 0, "forwarded", "")

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)
    plugin._pi_steer({"job": "j", "message": "do X", "interrupt": True, "no_wait": True})
    assert seen["argv"] == ["pi-supervisor", "steer", "j", "-i", "-n", "do X"]


def test_steer_requires_message(results_dir, monkeypatch):
    payload = json.loads(plugin._pi_steer({"job": "j"}))
    assert payload["usage_error"] is True


def test_bad_job_name_is_usage_error(results_dir, monkeypatch):
    for bad in ("-j", "a b", "", None, "j;rm"):
        payload = json.loads(plugin._pi_logs({"job": bad}))
        assert payload["usage_error"] is True, bad


def test_review_mutually_exclusive_pr_auto(results_dir, monkeypatch):
    payload = json.loads(plugin._pi_review({"job": "j", "pr": 5, "auto": True}))
    assert payload["usage_error"] is True
    assert "mutually exclusive" in payload["error"]


def test_review_requires_target(results_dir, monkeypatch):
    payload = json.loads(plugin._pi_review({"job": "j"}))
    assert payload["usage_error"] is True


def test_review_manual_args(results_dir, monkeypatch):
    seen = {}

    def fake_run(argv, **_kw):
        seen["argv"] = argv
        return subprocess.CompletedProcess(argv, 0, "armed", "")

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)
    plugin._pi_review({"job": "j", "pr": 12, "rounds": 3, "type_": "rebuttal"})
    assert seen["argv"] == ["pi-supervisor", "review", "j", "--pr", "12",
                            "--rounds", "3", "--type", "rebuttal"]


def test_review_auto_args(results_dir, monkeypatch):
    seen = {}

    def fake_run(argv, **_kw):
        seen["argv"] = argv
        return subprocess.CompletedProcess(argv, 0, "armed", "")

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)
    plugin._pi_review({"job": "j", "auto": True})
    assert seen["argv"] == ["pi-supervisor", "review", "j", "--auto"]


def test_review_bad_type_is_usage_error(results_dir, monkeypatch):
    payload = json.loads(plugin._pi_review({"job": "j", "pr": 1, "type_": "nope"}))
    assert payload["usage_error"] is True


def test_ack_shape(results_dir, monkeypatch):
    seen = {}

    def fake_run(argv, **_kw):
        seen["argv"] = argv
        return subprocess.CompletedProcess(argv, 0, "applied", "")

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)
    plugin._pi_ack({"job": "j", "event_id": "evt-1"})
    assert seen["argv"] == ["pi-supervisor", "ack", "j", "--event", "evt-1"]


def test_ack_requires_event(results_dir, monkeypatch):
    payload = json.loads(plugin._pi_ack({"job": "j"}))
    assert payload["usage_error"] is True


# --------------------------------------------------------------------------
# _verify_arm — the point of the plugin
# --------------------------------------------------------------------------

def test_verify_arm_success(results_dir):
    armed, info = plugin._verify_arm(good_dispatch_result(), str(results_dir))
    assert armed is True
    assert info["proc"] == "proc_abc123def456"
    assert info["notify_verified"] is True


def test_verify_arm_success_with_record(results_dir):
    write_proc_record(results_dir, "proc_abc123def456")
    armed, info = plugin._verify_arm(good_dispatch_result(), str(results_dir))
    assert armed is True
    assert info["record_verified"] is True


def test_verify_arm_record_with_notify_false_fails(results_dir):
    # The dropped-parameter failure, caught on disk after a fast exit.
    write_proc_record(results_dir, "proc_abc123def456", notify_on_complete=False)
    armed, info = plugin._verify_arm(good_dispatch_result(), str(results_dir))
    assert armed is False
    assert "RE-ARM REQUIRED" in info["why"]


def test_verify_arm_record_unreadable_fails(results_dir):
    path = results_dir / "proc_abc123def456.json"
    path.write_text("{not json")
    armed, info = plugin._verify_arm(good_dispatch_result(), str(results_dir))
    assert armed is False
    assert "unreadable" in info["why"]


def test_verify_arm_missing_background_started(results_dir):
    result = json.dumps({"output": "", "exit_code": -1,
                         "error": "Failed to start background process: no pty"})
    armed, info = plugin._verify_arm(result, str(results_dir))
    assert armed is False
    assert "did not start a background process" in info["why"]


def test_verify_arm_notify_false_is_the_dropped_parameter_failure(results_dir):
    result = json.dumps({
        "output": "Background process started",
        "session_id": "proc_deadbeef",
        "exit_code": 0,
        "notify_on_complete": False,   # the 2026-10-12 silent arm
        "hint": "process will exit silently...",
    })
    armed, info = plugin._verify_arm(result, str(results_dir))
    assert armed is False
    assert "notify_on_complete" in info["why"]
    assert "RE-ARM REQUIRED" in info["why"]


def test_verify_arm_notify_absent_fails(results_dir):
    result = json.dumps({
        "output": "Background process started",
        "session_id": "proc_deadbeef",
        "exit_code": 0,
    })
    armed, info = plugin._verify_arm(result, str(results_dir))
    assert armed is False


def test_verify_arm_subagent_demotion_fails(results_dir):
    result = json.dumps({
        "output": "Background process started",
        "session_id": "proc_deadbeef",
        "exit_code": 0,
        "notify_on_complete": False,
        "subagent_note": "this process's completion notice will NOT reach your parent",
    })
    armed, info = plugin._verify_arm(result, str(results_dir))
    assert armed is False
    assert "delegated-child" in info["why"]


def test_verify_arm_missing_session_id_fails(results_dir):
    result = json.dumps({"output": "Background process started", "exit_code": 0,
                         "notify_on_complete": True})
    armed, info = plugin._verify_arm(result, str(results_dir))
    assert armed is False
    assert "no session/process id" in info["why"]


def test_verify_arm_non_json_fails(results_dir):
    armed, info = plugin._verify_arm("segfault noise", str(results_dir))
    assert armed is False
    assert "not JSON" in info["why"]


def test_verify_arm_empty_fails(results_dir):
    armed, _ = plugin._verify_arm("", str(results_dir))
    assert armed is False


def test_verify_arm_persist_not_confirmed_warns(results_dir):
    result = good_dispatch_result(persist_on_release=False)
    armed, info = plugin._verify_arm(result, str(results_dir))
    assert armed is True
    assert "persist_warning" in info


# --------------------------------------------------------------------------
# arm_watch — dispatch plumbing and the no-dispatch fallback
# --------------------------------------------------------------------------

def test_arm_watch_success_path(results_dir):
    ctx = FakeCtx(good_dispatch_result())
    out = json.loads(plugin.arm_watch(ctx, "myjob", proc_results_dir=str(results_dir)))
    assert out["armed"] is True
    assert out["notify_verified"] is True
    assert out["proc"] == "proc_abc123def456"
    assert out["releases_on"][0] == "task_completed"
    # Flags are CODE: the dispatch args carry them even if the model drops them.
    name, args = ctx.dispatched
    assert name == "terminal"
    assert args["command"] == "pi-supervisor watch myjob -t"
    assert args["background"] is True
    assert args["notify"] is True
    assert args["notify_on_complete"] is True
    assert args["persist_on_release"] is True


def test_arm_watch_persist_false(results_dir):
    ctx = FakeCtx(good_dispatch_result())
    out = json.loads(plugin.arm_watch(ctx, "myjob", persist=False,
                                      proc_results_dir=str(results_dir)))
    assert out["armed"] is True
    assert ctx.dispatched[1]["persist_on_release"] is False


def test_arm_watch_no_dispatch_tool_refuses_with_manual_arm(results_dir):
    ctx = FakeCtx(with_dispatch=False)
    out = json.loads(plugin.arm_watch(ctx, "myjob", proc_results_dir=str(results_dir)))
    assert out["armed"] is False
    assert out["fallback"] == "manual-arm"
    assert "terminal(command=" in out["manual_arm"]


def test_arm_watch_dispatch_raise_refuses(results_dir):
    ctx = FakeCtx(raises=RuntimeError("registry exploded"))
    out = json.loads(plugin.arm_watch(ctx, "myjob", proc_results_dir=str(results_dir)))
    assert out["armed"] is False
    assert out["fallback"] == "manual-arm"
    assert "registry exploded" in out["error"]


def test_arm_watch_dispatch_silent_arm_reports_rearm(results_dir):
    ctx = FakeCtx(json.dumps({
        "output": "Background process started", "session_id": "proc_x",
        "exit_code": 0, "notify_on_complete": False,
    }))
    out = json.loads(plugin.arm_watch(ctx, "myjob", proc_results_dir=str(results_dir)))
    assert out["armed"] is False
    assert out["action"] == "RE-ARM REQUIRED"


def test_arm_watch_bad_job_never_dispatches(results_dir):
    ctx = FakeCtx(good_dispatch_result())
    out = json.loads(plugin.arm_watch(ctx, "-bad job", proc_results_dir=str(results_dir)))
    assert out["usage_error"] is True
    assert ctx.dispatched is None


# --------------------------------------------------------------------------
# slash command
# --------------------------------------------------------------------------

def test_piwatch_command_arms(results_dir):
    ctx = FakeCtx(good_dispatch_result())
    plugin._WATCH_CTX[0] = ctx
    try:
        out = json.loads(plugin._piwatch_command("  myjob  "))
    finally:
        plugin._WATCH_CTX[0] = None
    assert out["armed"] is True
    assert ctx.dispatched[1]["command"] == "pi-supervisor watch myjob -t"


def test_piwatch_command_empty_args(results_dir):
    out = json.loads(plugin._piwatch_command(""))
    assert out["usage_error"] is True


# --------------------------------------------------------------------------
# probe
# --------------------------------------------------------------------------

def test_probe_runs_plain_watch_without_t(results_dir, monkeypatch):
    seen = {}

    def fake_run(argv, **kw):
        seen["argv"] = argv
        seen["kw"] = kw
        return subprocess.CompletedProcess(argv, 0, "round_done rc=0", "")

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)
    payload = json.loads(plugin._pi_watch_probe({"job": "j", "timeout_s": 5}))
    assert seen["argv"] == ["pi-supervisor", "watch", "j"]  # NO -t, ever
    assert seen["kw"]["timeout"] == 5
    assert payload["probe"] is True


def test_probe_timeout_is_not_an_arm(results_dir, monkeypatch):
    def fake_run(argv, **kw):
        raise subprocess.TimeoutExpired(argv, 600)

    monkeypatch.setattr(plugin.subprocess, "run", fake_run)
    payload = json.loads(plugin._pi_watch_probe({"job": "j"}))
    assert payload["probe"] is True
    assert payload["ok"] is False
    assert "NOT an arm" in payload["note"]


# --------------------------------------------------------------------------
# registration shape
# --------------------------------------------------------------------------

def test_register_registers_all_tools_and_command():
    registered = {"tools": [], "commands": [], "schemas": {}}

    class RegCtx:
        def register_tool(self, name, schema, handler):
            registered["tools"].append(name)
            registered["schemas"][name] = schema

        def register_command(self, name, handler, description="", args_hint=""):
            registered["commands"].append(name)

    plugin.register(RegCtx())
    assert registered["tools"] == [
        "pi_status", "pi_steer", "pi_logs", "pi_review", "pi_ack",
        "pi_watch", "pi_watch_probe",
    ]
    assert registered["commands"] == ["piwatch"]
    # pi_watch must never accept a foreground/background flag from the model.
    assert set(registered["schemas"]["pi_watch"]["properties"]) == {"job", "persist"}