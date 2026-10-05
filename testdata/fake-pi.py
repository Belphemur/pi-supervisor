#!/usr/bin/env python3
"""Fake pi for testing internal/client: speaks just enough --mode rpc.

Behavior is selected by the prompt text so one script covers every case:
  TEST_STREAM      -> text_delta stream then agent_end (expect rc 0)
  TEST_ERROR       -> response{success:false} (expect rc 1)
  TEST_NOEND       -> emit deltas then exit without agent_end (expect rc 2)
  TEST_HANG        -> never emit anything (drives the timeout/abort path)
  TEST_ABORTDRAIN  -> on abort, emit agent_end (aborted turn) then handle the
                      next prompt normally (drain-and-continue)
  TEST_SLOW        -> TEST_STREAM but sleeping SECS=<n> first
  TEST_MKSESSION   -> create this run's session transcript in pi's
                      cwd-keyed sessions dir (like real pi does), then
                      TEST_STREAM; the supervisor's fsnotify session watcher
                      must adopt it without a poll interval
  TEST_FRAMELOG    -> keep the turn open and quiet (steer tests need a round
                      that stays live while frames arrive)

Every frame that arrives is appended to $FAKE_PI_FRAME_LOG (JSONL) when that
is set, so a test can assert what actually reached pi's stdin.
Anything else: TEST_STREAM.
"""
import json
import os
import sys
import time


def log_frame(frame):
    """Append a received frame to $FAKE_PI_FRAME_LOG when set."""
    path = os.environ.get("FAKE_PI_FRAME_LOG")
    if not path:
        return
    with open(path, "a") as fh:
        fh.write(json.dumps(frame) + "\n")
        fh.flush()


def out(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def delta(s):
    out({"type": "message_update",
         "assistantMessageEvent": {"type": "text_delta", "delta": s}})


def send_state():
    out({"type": "response", "command": "get_state", "success": True,
         "data": {"sessionFile": os.path.join(os.getcwd(), "sess-fake.jsonl"),
                  "sessionId": "fake-sess-1"}})


def emit_taskwatch(prompt):
    """ADR-0014 fixture: exercise the client's TaskUpdate correlation.

    Sends a get_state reply (sessionFile/sessionId), then the tool execution
    records the prompt names, then agent_end. Variants:
      TEST_TASKWATCH          -> one eligible TaskUpdate completed (taskId 7)
      TEST_TASKWATCH_NOISE    -> bash start/end, TaskUpdate status=in_progress
                                 completed-but-error end (no observation)
      TEST_TASKWATCH_ORPHAN   -> tool_execution_end with no start
      TEST_TASKWATCH_DUPEND   -> same toolCallId end twice (one observation)
    """
    variant = prompt.strip()
    send_state()

    def start(call, tool, args):
        out({"type": "tool_execution_start", "toolCallId": call,
             "toolName": tool, "args": args})

    def end(call, tool, res, is_error=False):
        out({"type": "tool_execution_end", "toolCallId": call,
             "toolName": tool, "result": res, "isError": is_error})

    if variant.endswith("TEST_TASKWATCH_NOISE"):
        start("c1", "bash", {"command": "ls"})
        end("c1", "bash", "ok")
        start("c2", "TaskUpdate", {"taskId": "7", "status": "in_progress"})
        end("c2", "TaskUpdate", "ok")
        start("c3", "TaskUpdate", {"taskId": "8", "status": "completed"})
        end("c3", "TaskUpdate", "Task #8 not found", True)
    elif variant.endswith("TEST_TASKWATCH_ORPHAN"):
        # End with no recorded start: must be ignored.
        end("c9", "TaskUpdate", "ok")
    elif variant.endswith("TEST_TASKWATCH_DUPEND"):
        start("c5", "TaskUpdate", {"taskId": "5", "status": "completed"})
        end("c5", "TaskUpdate", "ok")
        # Duplicate re-delivery of the same execution end.
        end("c5", "TaskUpdate", "ok")
    else:
        start("c7", "TaskUpdate", {"taskId": "7", "status": "completed"})
        delta("marked task 7 complete ")
        end("c7", "TaskUpdate", "ok")
    delta("done ")
    out({"type": "agent_end"})


def emit(prompt):
    if "TEST_FRAMELOG" in prompt:
        # Deliberately silent and never agent_end: the round stays live so a
        # steer can be delivered into it.
        return
    if "TEST_TASKWATCH" in prompt:
        emit_taskwatch(prompt)
        return
    if "TEST_MKSESSION" in prompt:
        # pi writes its session transcript asynchronously into the
        # cwd-keyed sessions dir: $HOME/.pi/agent/sessions/--<munged>--,
        # munged exactly like job.MungedSessionsDir (leading / stripped,
        # "/" -> "-"). Create it a beat AFTER the launch so the watcher
        # (armed pre-spawn) sees the create event.
        home = os.environ.get("HOME") or os.path.expanduser("~")
        munged = "--" + os.getcwd().lstrip("/").replace("/", "-") + "--"
        sdir = os.path.join(home, ".pi", "agent", "sessions", munged)
        os.makedirs(sdir, exist_ok=True)
        sess_path = os.path.join(sdir, "sess-%d.jsonl" % os.getpid())
        with open(sess_path, "w") as fh:
            fh.write('{"type":"session","id":"fake"}\n')
            fh.write('{"type":"message","message":{"role":"assistant",'
                     '"content":[{"type":"text","text":"TEST_MKSESSION_MARKER"}]}}\n')
        time.sleep(0.3)
        for w in ("hello ", "from ", "fake ", "pi"):
            delta(w)
            time.sleep(0.05)
        out({"type": "agent_end"})
    elif "TEST_STREAM" in prompt:
        # Echo any MARKER_ token so the supervisor's marker gate can see it
        # in the run log, exactly like a real agent would print the token.
        for tok in prompt.split():
            if tok.startswith("MARKER_"):
                delta(tok + " ")
        for w in ("hello ", "from ", "fake ", "pi"):
            delta(w)
            time.sleep(0.05)
        out({"type": "agent_end"})
    elif "TEST_ERROR" in prompt:
        out({"type": "response", "success": False, "error": "synthetic failure"})
    elif "TEST_NOEND" in prompt:
        delta("partial")
        sys.exit(3)
    elif "TEST_HANG" in prompt:
        time.sleep(3600)
    elif "TEST_ABORTDRAIN" in prompt:
        time.sleep(3600)
    elif "TEST_SLOW" in prompt:
        secs = 2
        for kv in prompt.split():
            if kv.startswith("SECS="):
                secs = int(kv.split("=", 1)[1])
        time.sleep(secs)
        delta("slow done")
        out({"type": "agent_end"})
    else:
        delta("default")
        out({"type": "agent_end"})


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            frame = json.loads(line)
        except ValueError:
            continue
        log_frame(frame)
        t = frame.get("type")
        if t == "prompt":
            emit(frame.get("message", ""))
        elif t == "get_state":
            # ADR-0014: the client requests session identity; answer here so
            # the fixture exercises the real correlated-reply path.
            send_state()
        elif t == "abort":
            # The aborted turn ends here; the client may then deliver a held
            # interrupt message, which we handle on the next loop iteration.
            out({"type": "agent_end"})
            sys.stderr.write("[fake-pi] aborted turn ended\n")
            sys.stderr.flush()


if __name__ == "__main__":
    main()
