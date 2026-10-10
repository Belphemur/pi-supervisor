# ADR-0018: Review campaigns run in their own session

Date: 2026-10-07
Status: Accepted

## Context

A review campaign round is "just a round": the loop launches/resumes pi the
same way a build round does. Because the job's `session_path` still pointed
at the BUILD session when the campaign started, every campaign round RESUMEd
that session — with `cont`, the build continuation prompt, on top.

That poisoned the campaign in two stacked ways, observed live on
`mealime-search3` (flambette#58, 2026-10-07):

1. The build session's own context ends with the completion narrative —
   "All work is verified complete — emitting completion marker". Every
   review round opened with a model that had just told itself the work was
   done, re-verified old work, and exited (~40s rounds, zero threads
   addressed, zero commits).
2. The build `cont` prompt asks to CONTINUE THE BUILD — meaningless in a
   review round.

Five rounds burned; `review_exhausted` with every thread still open. The
workaround (a fresh job re-briefing inherited state) works but duplicates
state and loses the campaign machinery.

## Decision

**A review campaign gets its own session and its own brief.**

- `job.review_brief` (path): the entry prompt for a campaign's fresh
  session. Typically: the PR, the worktree, what the build delivered, and
  the review contract (the shim's six verbs via the injected
  `pi_supervisor_review` skill).
- At ARM time — both the manual `review <job> --pr N` and the auto gate
  trigger — when `review_brief` is set, the daemon clears `session_path`
  and persists the clearing BEFORE the first round reads the job. The
  campaign's round 1 LAUNCHes a fresh session seeded with the review brief;
  every later campaign round resumes THAT session. Invariant 1 (never
  re-LAUNCH) holds WITHIN the campaign — one campaign, one session.
- `round()` selects the prompt: LAUNCH + `reviewing` ⇒ `review_brief`
  (never the build brief — it asks for work the campaign must not redo).
- Without `review_brief` (AMENDED 2026-10-10, owner directive): the daemon
  GENERATES a default review brief at arm time —
  `~/.pi/supervisor/briefs/<job>_review_brief.md` — persists it on the job
  def, clears the build pin, and the campaign LAUNCHes with it. The build
  continuation is NEVER sent to a review round: the earlier legacy fallback
  (resume the build session with `cont`) reproduced the exact poison this
  ADR describes — the agent's context says the work is complete, so it
  re-verified and exited without addressing threads, and the review never
  started (mealime-redesign, flambette#68). The generated brief is generic
  and deterministic; PR-specific state is read LIVE every round via the
  shim's `list_threads`, and the daemon's mandatory-reply contract is
  wrapped around the file by `writeCampaignRoundPrompt`. An operator brief
  always wins when present.
- The build session file is never deleted — it stays the build phase's
  record.
- A stop mid-campaign + watch-driven resume (ADR-0017) resumes the
  campaign's own session: the clearing happens at ARM only, never on a
  plain Start.

### Rejected

- Rewriting/clearing the marker or DCP state in the resumed build session:
  mutating pi's transcript/state to change what the model believes is
  fragile and fights the session format.
- Always launching fresh with the build `cont` as the prompt: a review
  round's entry prompt must describe REVIEW work; reusing a build prompt is
  how the second poison got in.

## Consequences

- Campaign rounds start with a clean context and a brief that says "address
  the open threads" — the 40-second re-verify spiral is structurally gone.
- `session_path` on a job now means "the CURRENT phase's session": the
  build session until the first campaign arm, the campaign session after.
  Status/`session_bytes` follow the current session.
- Existing jobs without `review_brief` no longer need one to run a working
  campaign: arm time generates the brief and gives the campaign its own
  session (amendment above). The build continuation path for review rounds
  is structurally unreachable — `round()` REFUSES a reviewing resume with
  no brief instead of falling back to `cont`.
- Each re-armed campaign (including manual re-entry after a spent campaign)
  starts another fresh session; the previous campaign's session file
  remains on disk as its record.
