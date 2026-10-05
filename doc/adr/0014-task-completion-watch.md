# ADR 0014 — TaskUpdate completion notifications enriched from pi-tasks JSON

Date: 2026-10-05
Status: Accepted — owner-approved v1 scope; implementation pending
Author: Antoine Aflalo (Belphemur), Hermes Agent

## Context

Long-running delegations use `@tintinweb/pi-tasks`. The owner wants watch to
report a completed task with its actual information, not just a task ID or a
model-authored summary. The approved first case is explicit `TaskUpdate` calls.

The installed pi 1.0.3 RPC interface emits `tool_execution_start` (toolCallId,
toolName, args) and `tool_execution_end` (toolCallId, toolName, result, isError).
The existing Go client reads this stream but currently ignores these events.
The installed pi-tasks 0.9.0 stores `{nextId, tasks: [...]}` snapshots and saves
using temporary-file + rename. Task IDs are strings and are only list-local.
Its statuses are `pending`, `in_progress`, `completed`.

A TaskUpdate requesting completion is not proof of completion. In particular,
the installed plugin returns ordinary text for a nonexistent task without
setting isError. Conversely, automatic subagent completions mutate the store
without a TaskUpdate call. Completed records can also be automatically deleted.

Verified references (installed source, not a new dependency):

- pi: `docs/json.md` (tool execution events), `docs/rpc-commands.md` (get_state).
- pi-tasks: `src/index.ts` (resolveStoreTarget, TaskUpdate, subagent callbacks),
  `src/task-paths.ts`, `src/tasks-config.ts`, `src/task-store.ts`, `src/types.ts`,
  `src/auto-clear.ts`.
- This repository: `internal/client/client.go`, `internal/events/events.go`,
  `internal/supervisor/supervisor.go`, `cmd/pi-supervisor/main.go`.

## Decision

### 1. Ownership and scope

| Component | Owns |
|---|---|
| pi-tasks | Task mutations, task storage and path-selection semantics |
| RPC client | Correlating actual executions and identifying their session |
| Task-store reader | Resolving the active store and validating a snapshot |
| Supervisor | Stamping job/round and publishing the resulting observation |
| Existing event/control/watch path | Audit, fan-out, rendering and terminality |

The RPC call is the trigger; the task JSON is the source of information.
There is no continuous task-file watcher, replacement plugin, title cache or
second notification service. The supervisor does not write task files, create
them to enable monitoring, or change PI_TASKS/the plugin's configured scope.
The implementation stays in Go and reuses existing RPC framing and lifecycle.

### 2. Trigger, confirmation and identity

1. Record a `tool_execution_start` only when toolName is exactly `TaskUpdate`,
   args.taskId is a nonempty string, and args.status is exactly `completed`.
2. Wait for the matching `tool_execution_end` by toolCallId. Do not react to
   streamed tool-call arguments, partial results, prose, or transcript quotes.
3. A tool execution error emits no task_completed. Orphan end events and
   unrelated/non-completion calls cannot initiate a lookup.
4. After a non-error matching end, read the correct task store, find the exact
   ID, and require its current status to be `completed`.
5. Copy the task information into a `task_completed` event. Do not parse the
   plugin's success prose and do not re-read the file when rendering watch.

The event means "pi marked this task completed; the file reports completed at
observation time", NOT "the supervisor verified the implementation" or "this
call certainly caused a new status transition". A repeated successful call
for an already-completed task is a new observation. Duplicate delivery of the
same execution is not: dedupe by round/execution identity, not task ID alone.
Pending and dedupe state are scoped to the client round and cleared on teardown.

Session identity and cwd must come from the actual running session, not a
newest-file heuristic or a search for a matching task ID across stores. The
first LAUNCH round must work before the supervisor's usual session-path pin
has happened. pi's correlated `get_state` reply supplies sessionFile/sessionId;
the session header supplies cwd. Reuse trusted existing session discovery
where it is sufficient; request RPC identity when needed. Auxiliary state
responses must not be mistaken for prompt errors or steer acknowledgements.
Do not block receipt of an RPC response while holding the reader that must
receive it. Never re-LAUNCH to discover identity.

Associate a pending completion with its session/store identity; a session
switch must not silently retarget an earlier task ID to a new list. If identity
cannot be established unambiguously, fail the lookup rather than guessing.
This integration does not relax the supervisor's never-fork resume invariant.

### 3. Resolve the store exactly once, in one adapter

Mirror the installed plugin's path policy in one tested resolver. It must use
the environment actually inherited by the child and the active session cwd.
Do not infer configuration from the shell that later runs watch.

Resolution precedence, matching pi-tasks:

1. `PI_TASKS=off`: memory-only; no readable store.
2. An absolute PI_TASKS value: that path.
3. A PI_TASKS value beginning with `.`: resolve relative to active cwd.
4. Any other nonempty PI_TASKS value: named list at `~/.pi/tasks/<value>.json`.
5. Otherwise merge `<agent-dir>/tasks-config.json` with
   `<cwd>/.pi/tasks-config.json` (project overrides global); taskScope defaults
   to `session`:
   - `memory`: no file.
   - `project`: `<cwd>/.pi/tasks/tasks.json`.
   - `session`: `<cwd>/.pi/tasks/tasks-<sessionId>.json`.
   - `session-global`: prefer an existing workspace session file; otherwise
     `<agent-dir>/tasks/sessions/<projectKey>/tasks-<sessionId>.json`.

`<agent-dir>` honors PI_CODING_AGENT_DIR, defaulting to `~/.pi/agent`.
`projectKey` is pi-tasks' encoding of the absolute cwd: leading separator
removed, remaining path separators and colons replaced with `-`, surrounded
by `--`. Session scope requires a persisted session, as the plugin does.
Validate externally obtained identity components; task IDs are lookup keys,
not path fragments. Unknown/ambiguous scope must not select another session.

File lookup reopens the pathname after execution ends: an old open file
handle would continue reading the inode replaced by the plugin's atomic save.
Validate the envelope and selected record, including duplicate-ID ambiguity.
Ignore additional plugin fields for forward compatibility. Use bounded reads
and finite handling of malformed/oversized data; never block on a FIFO/device.
Do not repair, lock, delete or mutate the plugin's store.

### 4. Event shape and output

Extend the existing event envelope additively. `task_completed` carries the
usual job, round, session_path, worktree, timestamp and best-effort pr_url,
plus `tool_call_id`, `task_file`, and a structured `task` object:

- id, subject, description, status;
- optional activeForm and owner;
- blocks and blockedBy;
- createdAt and updatedAt (plugin Unix milliseconds, not completion timestamps).

Preserve these plugin field names in the task object. Do not copy arbitrary
metadata in v1: it may contain large subagent results or unrelated payloads.
No fabricated title, description, timestamps or verification claim. Bound
payload sizes and visibly mark any display truncation; never truncate the
whole encoded event into invalid JSON or exceed the watch scanner budget.

Human watch output names the job, task ID, subject and description, owner when
present, and says the job remains running. Other task fields remain available
in the structured event. Escape terminal control characters in human output.
Task text is data, never executable instructions or a completion marker.

All task events go through the same supervisor emission funnel as lifecycle
events: one audit append, journal fact and broker publication. Refactor that
funnel if necessary rather than adding a parallel event publisher. Journal
lines contain identifiers/outcome, not task descriptions or arbitrary payloads.

### 5. Failure and lifecycle behavior

An eligible completed execution whose lookup cannot confirm the task produces
one non-terminal `task_lookup_failed` diagnostic with the task ID, tool call
ID, safe source context and a machine-readable reason. Distinguish missing
identity/store, unreadable/invalid data, missing/ambiguous task and task no
longer completed. New reason values belong in the existing `internal/fault`
vocabulary with its inventory/exit-code tests, not a second reason registry.
No raw task file contents or credentials in diagnostics.

Both new events are NON-TERMINAL. Ordinary watch returns on the first event and
prints re-arm/status instructions; watch --terminal prints progress and keeps
waiting. Terminal prechecks and exit codes retain their current behavior.
No new watch replay/cursor or guarantee of delivery between re-arms is added.
Existing slow-subscriber drops remain possible; the audit retains published
observations subject to the existing best-effort write contract.

Capture task info promptly. If auto-clear, reopening, or replacement wins the
race before the read, report the failed confirmation rather than recovering
another task or inventing a historical result. Once copied into the event,
later task deletion cannot erase the captured information.

Lookup failure must not fail the pi round, alter its RC, create instant-exit
strikes or affect the marker/report completion gate. Task work must not hold
runner/global locks during I/O or stall RPC control/abort handling. Bound any
queue/worker lifetime, surface overload rather than silently misattributing,
and finish or diagnose accepted pending observations before the job's terminal
event closes its watch. No late task event from an old round may be attributed
to a new one. Stop/shutdown remain bounded and preserve process-group handling.

### 6. Verification contract

Tests must exercise the connected path, not just a new parser:

- RPC start/end correlation, interleaving, duplicate end, unmatched IDs,
  execution error, non-completion calls and unrelated tools.
- Successful snapshot enrichment and immutable event data after deletion.
- No false completion for not-found (including a non-error tool result),
  malformed/missing file, pending/reopened task, duplicate IDs or bad identity.
- Resolver parity for default/resume/first LAUNCH, PI_TASKS overrides, memory,
  project, session-global legacy precedence, agent-dir overrides and two
  simultaneous sessions reusing the same task ID.
- Atomic rename, bounded oversized reads, unsupported file types and a
  missing source during lookup. No task-store writes by the supervisor.
- Real client -> supervisor -> control socket -> watch integration: enriched
  non-terminal event, ordinary-watch exit/footer, --terminal staying alive,
  and observation ordering relative to terminal events. Pin both daemon and
  CLI terminal classification; use one shared classification where possible.
- Stop/abort/round teardown under -race; all existing regressions stay green.
- An isolated, opt-in real-pi smoke using native TaskCreate/TaskUpdate and the
  candidate binary. Do not replace or restart the live supervising daemon.
  Record raw evidence and distinguish a skipped live probe from a pass.

Run build, vet, go fix -diff (empty), gofmt -l (empty), all tests with -race,
and golangci-lint. Reconcile this ADR and operator docs with the shipped code.

## Consequences and trade-offs

- DRY: the JSON record, not a cached title or parsed success sentence, owns
  task information. One resolver localizes unavoidable cross-language plugin
  compatibility knowledge, and one event funnel serves all consumers.
- SOLID over KISS: correlate executions, isolate sessions, validate inputs and
  bound concurrency even though blindly forwarding tool arguments is shorter.
- KISS: no custom extension, new daemon, polling loop, replay protocol or task
  mutation API for this scope. Existing watch delivery remains unchanged.
- Costs: file-path/schema compatibility must track pi-tasks; snapshot races
  are explicitly observable and task history is not guaranteed.

## Non-goals

Automatic subagent completions without TaskUpdate; detecting every task state
change; plugin modifications; independent proof that code/tests succeeded;
turning task completion into job completion; migrating task storage; adding
status dashboards; altering restart/session ownership; durable exactly-once
notifications or watch replay. A later comprehensive mutation-event/bridge
extension design may supersede the trigger without duplicating task ownership.
