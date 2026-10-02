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


def emit(prompt):
    if "TEST_FRAMELOG" in prompt:
        # Deliberately silent and never agent_end: the round stays live so a
        # steer can be delivered into it.
        return
    if "TEST_STREAM" in prompt:
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
        elif t == "abort":
            # The aborted turn ends here; the client may then deliver a held
            # interrupt message, which we handle on the next loop iteration.
            out({"type": "agent_end"})
            sys.stderr.write("[fake-pi] aborted turn ended\n")
            sys.stderr.flush()


if __name__ == "__main__":
    main()
