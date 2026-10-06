# ADR 0006 — Pull-request URL as run metadata (best-effort transcript scrape)

Date: 2026-10-03
Status: Accepted
Author: Antoine Aflalo (Belphemur), Hermes Agent

## Context

The supervisor's whole point is that the operator can ask it what happened
(`status`) instead of grepping a session transcript. But the single most
wanted fact after a long campaign — "where is the PR?" — was not in the
status at all. The operator had to tail the session JSONL or ask the agent.

Two naive fixes were rejected up front:

- **Ask GitHub.** No token, no network, no `gh`: the daemon must stay a local
  process with no credentials and no failure mode that can hang a round.
- **Ask the agent.** A round that ends at its marker has no turn left to
  answer a question, and a round that dies has no one to ask.

## Decision

**The URL is scraped from the transcript the daemon already tails.**

- **Reuse the existing per-round tail.** `internal/stall`'s detector already
  reads the session JSONL incrementally for the CI-stall signal. A sibling
  `prRe` in the same read path costs no new file watching, no new goroutine and
  no new lifecycle: the watcher goroutine `watchCIStalls` already runs for the
  whole round. Detection result is stored on the runner under `r.mu`, keeping
  the `s.mu -> r.mu` lock order (ADR-0005's rule: never hold a lock across a
  wait).
- **Regex pinned to the real signal.**
  `https://github\.com/[^/" ]+/[^/" ]+/pull/[0-9]+`, verified against a live
  session containing `https://github.com/Belphemur/XPoint/pull/184`. The
  digits are required: a truncated `…/pull/` fragment (a half-flushed JSONL
  line, or an agent still typing the URL) must not match, or status would
  advertise a link that 404s.
- **Scanning over raw JSONL bytes**, exactly like `ciRe`, so JSONL schema
  drift cannot break it.
- **First URL of the round wins**, because the detector starts at the file end
  when the round begins — content before that is an earlier round's PR and is
  historical by construction.
- **One final scan on watcher exit.** The agent writes its PR link as it
  exits, which can land after the last regular tick; without the final poll
  the URL would surface one round late.
- **Surfaces**: `job.State.PRURL` (persisted, so it survives a daemon
  restart) → `job.Status.PRURL` and every event's `pr_url`
  (`done`/`fatal`/`stopped`/`instant_exit`, plus the watch precheck event).
  `round_done`'s `text` gains ` | pr <url>`, `status <job>` carries `pr_url`
  as a JSON field, and the watch footer prints the URL. (Amended: the CLI's
  trailing `pr <url>` status line was removed — status stdout is exactly one
  JSON document, jq-parseable.)

## Consequences

- **It is a best-effort scrape, and it may legitimately be empty.** An agent
  that opens a PR and never links it (or links it in a form other than a
  GitHub URL — `gh pr view` output, a bare `#184`) leaves `pr_url` empty.
  "" means *not linked in the transcript*, never *no PR exists*.
- **Eventually consistent**: mid-round the value can lag the transcript by one
  poll tick. Terminal events are authoritative because the watcher has done
  its final scan before the round is classified.
- A PR opened in a later round overwrites the earlier value; a round with no
  new URL keeps the last known one.
- No new dependency (`regexp` only), no extra process, no auth.

## Alternatives considered

- **A `pr_url` field in the job JSON**, filled in by the operator. Rejected:
  it is exactly the manual step this is meant to remove.
- **A new dedicated tailer goroutine.** Rejected: the stall watcher already
  owns the read loop for the round's whole life; a second reader would double
  the I/O and need its own rotation handling for no extra signal.
- **Matching `gh pr create` output too.** Rejected for now: the URL form is
  the stable, machine-readable one; broadening the regex trades false
  positives (a PR mentioned in passing, a linked *other* PR) for coverage that
  the operator can always get by tailing the transcript.