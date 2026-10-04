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
and a bounded round budget. Nothing about it was urgent, which is exactly what
makes it a good probe: when the tooling is under test, the work should not also
be under deadline.

Three pi-supervisor jobs ran against this repo: `journal` (implement),
`journalverify` (adversarially verify), `journalfix` (repair what verify found).

## Decision

### What the supervisor got right

Recorded because the failures below are more informative without a baseline.

- **The completion gate worked on a real agent.** pi emitted `ALL_JOURNAL_DONE`
  in an assistant message and the daemon detected it in the **transcript**
  (ADR-0011), reporting `marker "ALL_JOURNAL_DONE" detected in session
  transcript — run is over`, rc=0, duration 1736s — one round, no wasted budget.
- **Resume-by-session held.** Round 1 LAUNCHed, captured `session_path`, and the
  run never re-launched.
- **The job was self-contained.** It produced a commit on its own branch, pushed
  it, and opened PR #2 without being told the branch name.
- **Adversarial verification was genuinely adversarial.** It returned
  `VERDICT: FAIL (3)` against work I had already hand-checked and partly
  declared sound, and two of its findings were defects I had missed entirely
  (F2, F3 below). Self-review by the implementer would have found neither.
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

Reproduced **twice**, independently: on `master`'s own CI run, and again on PR #2,
whose branch was cut before the fix landed. Same error both times.

**Lesson (DRY):** a gate is only as good as its *installation* path. The local
gate tested "does lint pass with the binary I have"; the CI gate tested "can this
binary be obtained". Those are different questions and only one was being asked
locally.

**Fixed** in `3a15bb5` → `golangci/golangci-lint-action@v7`, which pins the
version without the stale hash comparison.

### F3 — Two defects in the logging feature that only adversarial review found

Both were hand-checked and partly declared sound before the verifier ran. Recorded
because being wrong about that is the useful part.

**(a) `event=daemon_ready` is never emitted.** `runDaemon` carries the comment
*"the journal line above is the queryable form"* — and the
`journal.L().Info("daemon_ready", …)` call was lost when the human banner was
restored during a lint round. The code references something that does not exist:

```
$ grep -rn daemon_ready cmd/ internal/     # (nothing)
```

The READY transition — the one moment an operator asks "did the daemon come up?" —
was the only lifecycle event with no journal line, behind a comment promising one.

**(b) `-p warning` filters nothing.** Issue #1 requires a level prefix *"so
`journalctl -u pi-supervisor -p warning` filters usefully"*, and the PR body plus
`AGENTS.md` repeat the claim. Measured on the live unit:

```
$ journalctl --user -u pi-supervisor -o json | <PRIORITY histogram>
Counter({'6': 12})          # every daemon line is 6 = info
```

journald derives PRIORITY from the **stream**, not from line text, so the
` WARN `/` ERROR ` tokens are just characters. This is structural, not a coding
slip: a real priority needs a `/dev/log` datagram, which issue #1 lists under
non-goals.

**Resolution (owner decision):** split the streams — INFO to stdout, WARN and
ERROR to stderr — via zerolog, which is the library's documented systemd pattern.
This stops `-p warning` returning INFO chatter. It does **not** create distinct
priorities (journald uses the unit's priority for stderr too), so the residual
limit is documented wherever the claim appears, along with the incantation that
works today:

```bash
journalctl --user -u pi-supervisor -o cat | grep ' WARN \| ERROR '
```

The general lesson: **an issue requirement can be unmeetable as written while its
intent is still worth honouring.** The honest move is to implement the intent,
then state plainly which part remains out of reach and why.

### F4 — One refusal path logs no specific reason, and the enum was only half-wired

**Severity: medium. Class: partial acceptance of issue #1 criterion 2.**

`internal/fault` defines a closed enum — `unknown_job`, `not_running`,
`already_running`, `already_done`, `bad_request`, `bad_json`, `unknown_cmd`,
`unsupported`, `refused` — and `logResponse` forwards `resp.Reason` as `reason=`.
Right shape. But only *some* handlers tag their errors:

```
WARN control event=request_refused cmd=status job=nope reason=refused err="unknown job \"nope\""
```

`reason=refused` is a constant emitted for every untagged error, so it carries no
information: it cannot be filtered, counted, or alerted on, and the specific
cause survives only in the `err=` **prose**. `start`/`stop` refusals *do* log the
specific symbol, which proves the design works and this is an omission.

I initially found only `Status`; the verifier found `Status`, `Steer` (×2) *and*
`Logs`. **A closed enum is only closed if every producer populates it** — and an
untagged producer degrades to the least informative member at exactly the moment
the operator is looking.

### F5 — A vacuous test that claimed to protect a real invariant

**Severity: low. Class: test quality.**

`TestConcurrentLinesDoNotInterleave` cannot fail: all eight goroutines call
`Subsys("job")`, which is one logger with one handler, so deleting the shared
write lock still passes. Production has three handlers (`daemon`, `job`,
`control`) writing to one sink — the shape the test never exercises.

The invariant itself is real, proven by a throwaway probe:

```
shared=true   lines=160 spliced=0     # 160 records -> 160 intact lines
shared=false  lines=138 spliced=0     # 160 records -> ~136: merged/lost
```

So the lock is load-bearing and the test that claims to protect it would not catch
its removal. The other formatting tests are *not* vacuous: restoring a trailing
newline fails four of them.

### F6 — A stale-binary hypothesis that was wrong

**Severity: none. Class: near-miss worth recording.**

Mid-run, `session_bytes` read identically across two checks ten minutes apart,
which looked like a wedged round. It was not: pi was inside a long tool call and
the transcript had not flushed. The job completed normally at 1736s.

The tempting move was to "fix" the stall detector. There was nothing to fix.
**`session_bytes` is a flushed-write counter, not a liveness signal** — the
liveness signals are the client's pid and `last_update`.

### F7 — The brief and the job config contradicted each other

**Severity: none (no product defect). Class: operator error, caught by the tool.**

The verification job ended `fatal`: *"round cap 4 reached without marker"*. The
cause was mine:

```
/tmp/pi_journal_verify_brief.md:92
  "Do NOT emit any completion marker. This job has no marker and does not need one."

~/.pi/supervisor/jobs/journalverify.json
  "marker": "VERIFY_DONE"
```

I told the agent **not** to emit a marker, configured the job to **require** one,
and it spent four rounds on adversarial verification instead of stopping. The
agent obeyed the brief exactly; the supervisor then correctly refused to call a
run `done` whose completion signal never arrived. The 261-line report — the actual
deliverable — was complete and unaffected.

**Two lessons, and the second matters more:**

1. **A brief and its job definition are one contract.** They live in two places,
   so they drift, and nothing checked them. The failure mode is expensive *and*
   invisible: the work is fine, the run is still reported as a failure.
2. **The supervisor did exactly its job.** ADR-0011's gate refused to infer
   completion from "the agent seems done" — the same false-delivery class
   ADR-0005 exists to prevent. A lenient gate would have recorded a job that
   stopped early as `done`.

Contrast with the `journal` job, which ended `done` cleanly at rc=0 on identical
machinery: that brief *did* carry an explicit "emit the marker verbatim in your
final assistant message" rule. The difference was entirely in the instructions.

**Mitigation:** when a job needs no marker, either give it an empty one and say
completion is the report file alone, or tell the agent to emit it. Never brief
"do not emit X" while configuring the job to require X.

## Consequences

- **A supervisor must be able to report that it did not do the thing.** F1 is the
  argument: every tool in this chain now distinguishes "did it" from "said it
  did".
- **A gate's install path is part of the gate.** F2 only surfaced because the work
  ran where the artifact had never been fetched — and reproduced on two branches.
- **Adversarial review earns its cost.** F3's two defects survived my own hand
  check. The implementer reviewing itself would have found neither.
- **An issue requirement can be unmeetable while its intent is not.** F3(b) was
  fixed as far as the constraint allows, with the limit stated rather than
  papered over.
- **A closed enum is only closed if every producer populates it.** F4.
- **Instructions are part of the system under test.** F7: the same daemon, the
  same job shape, opposite outcomes, decided entirely by the brief.

## Follow-ups

1. ~~Wire `Status`, `Steer` and `Logs` refusals to `fault.New` — F4.~~
   **DONE** (`fault` is wired since the journal ADR).
1b. ~~One wire vocabulary for refusal reasons (DRY): `fault.Kind` vs
   `review.Reason` for the same `Response.Reason` field.~~ **DONE** in
   `04944d3` (PR "one refusal vocabulary"). `internal/fault.Kind` is the
   single source of truth; `review.Reason` is a type **alias** of it, and
   `ExitCode()` moved onto `fault.Kind` as a data table
   (`exitCodes`), so a Kind added without a mapping fails a test instead of
   inheriting the runtime-refusal fallback. Every value ADR-0012 §2.4
   published is byte-identical. The one migration: `fault.KindNoLiveRound`
   was `"no_live_round"` and the steer path really did emit it — it is now
   `"no-live-round"`, the spelling the review shim already matched, so a
   single condition has a single spelling everywhere. `bad_request` and
   `usage` are kept distinct (both published, both "change the call") and
   fault_test pins that they exit alike. Guards:
   `TestNoDuplicateSpellings`, `TestLegacyNoLiveRoundSpellingIsRetired`,
   `TestEveryKindHasAnExitCode`, `TestKindValuesAreStable`,
   `internal/review/reason_test.go`.
2. Emit `daemon_ready`; document the stream-split limit — F3.
3. Replace the vacuous concurrency test with the three-handler shape — F5.
4. Make `install.sh` part of the repo's own CI smoke test, so the install path is
   exercised somewhere other than a deploy.
5. Consider pairing `session_bytes` with a liveness field in `status`, so a
   frozen counter is not mistaken for a hung round — F6.
6. Have the supervisor cross-check a job's marker against its brief, or support an
   explicit "no marker" job shape — F7.

## Credit

Antoine Aflalo (Belphemur), Hermes Agent