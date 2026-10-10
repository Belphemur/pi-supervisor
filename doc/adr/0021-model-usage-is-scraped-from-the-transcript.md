# ADR-0021: Model usage is scraped from the session transcript

Date: 2026-10-09

## Status

Accepted. Implementation follows this ADR.

## Context

The daemon supervises long-running pi delegations, but it never answers a
basic operational question: **which model is actually answering?** The job
definition carries a `provider`/`model` *request*, yet the session transcript
proves what answered — and on routed providers (openrouter pools,
opencode-go) the transcript's `model` field is frequently NOT what the job
asked for. Round 3 of a job can silently run on `step-5-preview-free` while
the brief believed it was talking to something else. When a run degrades,
half the diagnosis is "what model produced those 12 rounds?" — and today that
answer requires hand-grepping the JSONL.

Every assistant record in the session JSONL already carries the truth:

```json
{"type":"message","message":{"role":"assistant","provider":"opencode-go",
 "model":"step-5-preview-free","usage":{...}}}
```

## Decision

1. **The transcript is the source of truth.** Model identity is scraped from
   assistant records of the session JSONL — the same surface the marker gate
   (ADR-0011) and the PR scrape (ADR-0006) already read. A job-definition
   request is intent, never evidence.
2. **Incremental JSONL parsing, not regex.** Like the PR scrape, a per-runner
   scanner reads only bytes appended since its last call (offset, inode
   rotation check). Unlike the byte-regex scrape, it parses every COMPLETE
   line with `encoding/json` — an incomplete trailing line is carried to the
   next read (capped, same discipline as the client's control reader) — and
   DISCARDS anything it does not recognize: non-JSON noise, non-assistant
   roles, records without a model. Only an assistant record's
   `message.role == "assistant"` with a non-empty `message.model` counts,
   so field reordering, unknown fields, and quoted model text in briefs,
   steers, and toolResults can never produce a false identity. The scanner
   is the ONE component that derives model identity; nothing else
   re-derives it (the marker funnel rule).
3. **Two fields, one list — plus the latest cumulative usage.** State and
   status carry:
   - `models_used` — the ordered, deduplicated list of every model that has
     answered at least one assistant record this run (first-seen order).
   - `current_model` — the LAST model seen (what is answering right now,
     as of the last scan).
   - `total_tokens` / `total_cost` — the LATEST assistant record's
     `usage.totalTokens` / `usage.cost.total` (owner directive 2026-10-10).
     pi already reports these as RUNNING totals per record, so the scanner
     keeps the latest — it never sums, which would double-count every
     cached turn.
   All reset on `restart --fresh` with the rest of the run-scoped state.
4. **Surfaced where an operator or watcher needs it.** `status <job>`
   carries `models_used` + `current_model`; the round-boundary log line and
   the live-round model watcher log changes; `done` event info names the
   answering model (and the full list when more than one). A `""` means
   "nothing scraped yet", never "no model".
5. **Never a gate.** Model identity is diagnostic. It must not influence
   completion, budget, or classification — the completion gate (ADR-0020)
   and the campaign budget read reports and text, not provider metadata.
   A weird model name in the list is a finding for the operator, not a
   failure mode for the daemon.

## Consequences

- `status` answers "what model is this run actually using" with zero new
  network calls and no client changes — the transcript already has the data.
- Multi-model runs (provider failover, operator swaps mid-run) are visible
  in the list the moment the next record lands.
- The scanner reuses the PR scanner's proven incremental-read discipline
  (offset, rotation check) but parses COMPLETE JSONL records with
  `encoding/json`, discarding anything unrecognized — see decision 2.
