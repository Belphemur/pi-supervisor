# ADR-0013: Findings from dogfooding the review loop on pi-supervisor itself

## Status

Accepted as a **record of observed behaviour**, not a design decision. Every
finding below was reproduced on this machine; each entry says how. Two defects in
the *tooling* were fixed in the same pass that found them, and are marked.

Scope: what a supervised pi run on pi-supervisor's own repository revealed about
the supervisor, when the work was small and the CI gate real.

## Context

ADR-0012 built `pi-supervisor review` and was verified with unit tests, lint
gates, and **read-only plus write live tests against a real PR**. What it did not
do is run *itself* on a real feature end to end. Unit tests prove the parts; a
supervised multi-round run proves the whole.

So: issue #1 ("make the daemon's diagnostics readable in `journalctl`") became
the first real job — a small, well-scoped feature on a repo with CI, a real PR,
and a bounded 4-round budget. Nothing about it was urgent, which is exactly what
makes it a good probe: when the tooling is under test, the work should not also be
under deadline.

Two pi-supervisor jobs ran: `journal` (implement, 1 round, rc=0) and
`journalverify` (adversarially verify, ≤4 rounds).

## Decision

### What the supervisor got right

Recorded because the failures below are more informative without a baseline.

- **The completion gate worked on a real agent.** pi emitted `ALL_JOURNAL_DONE`
  in an assistant message and the daemon detected it in the **transcript**
  (ADR-0011), reporting `marker "ALL_JOURNAL_DONE" detected in session
  transcript — run is over`, rc=0, duration 1736s — one round, no wasted budget.
- **Resume-by-session held.** Round 1 LAUNCHed, captured
  `session_path`, and the run never re-launched.
- **The job was self-contained.** It produced a commit on its own branch, pushed
  it, and opened PR #2 without being told the branch name.
- **`watch -t` terminated correctly** and its footer named the exact next
  commands.

### F1 — `install.sh` reported success while serving a stale binary

**Severity: high. Class: false success.**

`install.sh` ran `systemctl --user enable --now`. That is a **no-op on an
already-active unit**: it enables the unit but never restarts a running one. The
rebuilt binary therefore never took effect, and then:

```
==> verification
active                                   # is-active: TRUE for the OLD process
error: daemon not reachable at /run/user/1000/pi-supervisor.sock: … connection refused
```

`enable --now` returned 0, `is-active` printed `active`, and the script exited
successfully — while a 10-hour-old process held the socket and the new code was on
disk only.

**Why it matters beyond the installer:** this is ADR-0005's exact failure class.
The system asserted a delivery it had not verified. An installer that cannot fail
is worse than no installer, because it converts "I did not deploy" into "I
deployed" in the operator's head.

**Fixed** in `a64bdb4` → verified `built=a9739a6 serving=a9739a6`:

1. `enable` then `restart` — `restart` is unconditional, so a new binary always
   takes effect.
2. Verification polls until the socket **answers**, rather than sleeping 2s and
   trusting `is-active` (the socket exists only after `LoadJobs` + `Serve`).
3. Verification compares the built commit against the version the **running
   binary reports**, and fails loudly on a mismatch. A stale process cannot
   report the new commit, so this check cannot be spoofed by the stale process
   itself.

### F2 — CI failed on its first run, for a reason no local gate could catch

**Severity: medium. Class: unpinned external artifact.**

The workflow installed `golangci-lint` with
`curl … install.sh | sh -s -- -b … v2.14.0`. That installer verifies the release
tarball against a **hardcoded sha256** which is now stale:

```
golangci-lint err hash_sha256_verify checksum for
  '/tmp/…/golangci-lint-2.14.0-linux-amd64.tar.gz' did not verify
  0cff1e23…  vs ab90aeb7…
```

Locally this can never happen: `/home/balor/go/bin/golangci-lint` is already
installed, so the local gate runs a working binary and the CI path — *downloading*
it — is exercised for the first time only in CI.

**Lesson (DRY):** a gate is only as good as its *installation* path. The local
gate tested "does lint pass with the binary I have"; the CI gate tested "can this
binary be obtained". Those are different questions and only one was being asked
locally.

**Fixed** → `golangci/golangci-lint-action@v7`, which pins the version without
the stale hash comparison.

### F3 — `Status()` returns an untagged error, so one refusal logs no specific reason

**Severity: medium. Class: partial acceptance of issue #1 criterion 2.**

The new `internal/fault` package defines a closed enum — `unknown_job`,
`not_running`, `already_running`, `already_done`, `bad_request`, `bad_json`,
`unknown_cmd`, `unsupported`, `refused` — and `logResponse` forwards
`resp.Reason` into the journal as `reason=`. That is the right shape.

But only *some* handlers tag their errors. `Start`, `Stop` and `Restart` wrap with
`fault.New(fault.KindUnknownJob, …)`; `Supervisor.Status` (supervisor.go:208) does
not:

```go
if !ok {
    return nil, fmt.Errorf("unknown job %q", name)   // untagged
}
```

so `KindOf` falls back to the generic `KindRefused` and the journal shows:

```
WARN control event=request_refused cmd=status job=nosuchjob reason=refused err="unknown job \"nosuchjob\""
```

`reason=refused` is not useful to `journalctl -p warning` triage: it cannot be
filtered, counted, or alerted on, and the specific cause survives only inside the
`err=` **prose** field. The specific cause is *machine-readable on the CLI* but
*not in the journal*, which is where an operator actually looks.

**Deliberately left unfixed.** It is a one-line change per handler, but changing
which reasons reach the journal is a decision about the operator-facing contract,
not a logging bug, so it wants its own commit. Recorded here so it is not lost.

### F4 — A stale-binary hypothesis was wrong, and reading it honestly matters

**Severity: none. Class: near-miss worth recording.**

Mid-run, `session_bytes` read identically across two checks ten minutes apart,
which looked like a wedged round. It was not: pi was inside a long tool call and
the transcript had not flushed. The job completed normally at 1736s.

The tempting move was to "fix" the stall detector. There was nothing to fix. The
lesson is narrow and worth keeping: **`session_bytes` is a flushed-write
counter, not a liveness signal** — the liveness signals are the client's pid and
`last_update`. An operator reading a frozen byte count should check the process
before reaching for the supervisor.

### Non-findings (checked, clean)

- **No secrets in the journal.** Grepped for `ghp_*`, `github_pat_*`,
  `BEGIN … PRIVATE KEY`, `Authorization: Bearer` across the whole journal window:
  zero matches.
- **Log volume is bounded.** A complete job lifecycle (start → round_start →
  round_end → backoff → job_fatal) produced **5** lines against issue #1's bound
  of 50.
- **The watchdog `STATUS=` still works**, and is arguably better than before:
  `StatusText=0 parallel pi session(s) running; FATAL: firmware-rebase,jlcheck,…`
  surfaces the fatal set without opening a log.

## Consequences

- **A supervisor must be able to report that it did not do the thing.** F1 is the
  argument: every tool in this chain now distinguishes "did it" from "said it did".
- **A gate's install path is part of the gate.** F2 only surfaced because the work
  ran somewhere the artifact had never been fetched.
- **A closed enum is only closed if every producer populates it.** F3 shows an
  enum that degrades to its least informative member at exactly the moment it
  matters. `KindRefused` as a fallback is correct; it should be visible as a
  fallback, not mistaken for a specific cause.
- **In-repo CI is now a merge gate** (ruleset `master gate`, id 24452137), so F2
  could not reach `master`: the first push ran the workflow, it failed, and the
  fix shipped as a second commit. The gate did its job on the run that created it.

## Follow-ups (not done here)

1. Tag every refusal in `Supervisor.Status` and `Logs` with `fault.New` — F3.
2. Make `install.sh` part of the repository's own CI smoke test, so the install
   path is exercised somewhere other than a deploy.
3. Consider whether `session_bytes` should be paired with a liveness field in
   `status`, so a frozen counter is not mistaken for a hung round — F4.

## Credit

Antoine Aflalo (Belphemur), Hermes Agent