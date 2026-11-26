# Interfaces and client behavior

The [R1 OpenAPI specification](../api/openapi.yaml) is the source of truth for
R1 HTTP paths, headers, request and response shapes, and status codes. This
document specifies their behavior (including replay, ordering and domain
failures), gives worked examples, and describes later releases. Change the
specification and the affected examples together; CI validates the OpenAPI
document and its examples. The broker envelope is not an HTTP endpoint.

These contracts apply to this engine, **not** to other engines.
[Test adapters](delivery.md) translate intent, not raw HTTP.
The [architecture](engine.md) owns transaction boundaries; the
[model profile](model-profile.md) owns model and form validation.

## Common HTTP contract

Base path `/v1`. Bodies are UTF-8 JSON. IDs are non-empty opaque strings;
times are RFC 3339 UTC with `Z`, durations below are milliseconds unless
explicitly named otherwise. `?` on a field below means optional; `|` means
alternatives. Reject unknown request fields, duplicate object keys, invalid
types and non-finite/out-of-profile numbers. No credentials or tenant header.

**Commands.** All mutating POSTs (including deployment, completion and
operator edits) require `Idempotency-Key`, 1..128 printable non-space ASCII
characters. Key space is shared across routes and transports. Normalize to
`(command kind, target IDs, JSON payload)`; compare objects without regard to
member order, numbers by value, arrays in order, strings exactly. Replaying
the same key and request returns the original operation ID and current
status; same key and different request -> HTTP 409 `IDEMPOTENCY_CONFLICT`.
A fresh key means a **new** command, not resource-level deduplication.

* New or replayed command: HTTP 202, `Location: /v1/operations/{operationId}`,
  body `{"operationId":"op-7","status":"PENDING"}` (status may already be
  terminal on replay). Never infer success from 202.
* `GET /operations/{id}` -> 200 `{operationId,status,result?,error?}`.
  `PENDING` has neither; `SUCCEEDED` has only `result`; `FAILED` has only
  `error`. Terminal outcomes are immutable. Retry transport failures with the
  same key, poll pending outcomes with backoff, use a **new** key for a
  corrected request.
* Error object: `{code,message,details?}`; admission/query failures use
  `{"error":{...}}` with 400 `INVALID_REQUEST`, 404 `NOT_FOUND` for unknown
  read target, 409 for an idempotency conflict, or 503 `UNAVAILABLE` when
  admission/read cannot be confirmed. A syntactically valid command with a
  missing target, invalid model, bad form values or a lost concurrency race
  instead resolves to `FAILED`; reading that operation still returns 200.
  Model diagnostics use `details.diagnostics: [{file,elementId?,field?,reason}]`.
  Field errors use `details.fields: [{key,reason}]`.
* Reads reflect committed state. Lists return `{items:[...],nextCursor:null|string}`;
  inputs `limit` (default 50, 1..200), `cursor?`. Order by creation time
  then ID ascending unless otherwise specified. Cursor means strictly after
  the last key for the same filters, not an open snapshot. Reject invalid/
  mismatched cursors. History, events, jobs claimed and aggregate reports
  have explicit ordering/limits below.

## Release 1: public API

Paths in tables are relative to `/v1`. The kind in parentheses is also the
broker-envelope `kind`. An operation result below is **not** the 202 body.

| Command route (kind) | Request body | Success result; important domain failure |
| --- | --- | --- |
| `POST /deployments` (`Deploy`) | `{files:[{name,content}]}`; `content` is the complete UTF-8 source as a JSON string, not a path or a URL | `{processKey,version}`. Each distinct successful Deploy publishes a new version even for identical source. `INVALID_MODEL` / `UNSUPPORTED_FEATURE`; no partial publication. |
| `POST /instances` (`Start`) | `{processKey,variables?}`; default `{}` | `{instanceId,processKey,version}` on creation, not on reaching a wait. `NOT_FOUND` if no head. |
| `POST /tasks/{taskId}/complete` (`CompleteTask`) | `{fields:{...}}` (possibly empty) | `{taskId,instanceId,revision}`. `NOT_FOUND`, `TASK_NOT_OPEN`, `INSTANCE_IN_INCIDENT`, `INVALID_FIELDS`. Invalid input never claims/closes the task. |

| Query | 200 response |
| --- | --- |
| `GET /definitions/{processKey}/versions` | Page of `{processKey,version,label,createdAt}` rows, ascending version; `label=CURRENT/DRAINING/DRAINED`. |
| `GET /instances/{instanceId}` | `{instanceId,processKey,version,status,revision,variables,createdAt,endedAt,incident}`. `status=RUNNING/INCIDENT/COMPLETED`; `endedAt` and `incident` nullable; incident is `{code,elementId,message}`. Terminal values remain readable. |
| `GET /tasks?instanceId=&status=` | Both filters optional, status defaults to `OPEN`; page of task summaries `{taskId,instanceId,elementId,version,status,createdAt,dueAt}`; `dueAt` nullable. |
| `GET /tasks/{taskId}` | Summary plus `{formId,components,closedAt,reason}`. `components` is the saved resolved view (including on terminal tasks); `closedAt` and `reason` nullable. `status=OPEN/COMPLETED/CANCELLED`; timer cancellation reason `TIMER` (R3 also `MIGRATED`). |

**Worked call sequence using the R1 fixture.** Deploy with `files` made from
the contents of the three model files; wait for `{processKey:
"Process_Collect_from_Form",version:1}` in a fresh store. Then:

```http
POST /v1/instances
Idempotency-Key: start-demo-1
Content-Type: application/json

{"processKey":"Process_Collect_from_Form","variables":{"input":"hello"}}
```

Poll the operation until `{"operationId":"op-7","status":"SUCCEEDED","result":
{"instanceId":"i-4","processKey":"Process_Collect_from_Form","version":1}}`.
IDs here are illustrative: take actual IDs from each response. List open tasks
for `i-4` until one appears; GET it to see `Caller says: hello`. POST
`{"fields":{"text_command":"terminate"}}` to `/v1/tasks/{returnedTaskId}/complete`
with a fresh key. After its operation succeeds, poll the instance: eventually
`status=COMPLETED`, `executionCount=1`, `decision="break"`. Do not replace
these waits with a fixed sleep.

### Broker adapter contract (port in R1, first real transport in R2b)

The delivery body is `{schemaVersion:1,key,kind,target,payload}`; `target`
contains **only** IDs from path placeholders (`{}` for Start and Deploy);
`payload` is exactly the command body from the route table. Example:

```json
{"schemaVersion":1,"key":"submit-42","kind":"CompleteTask","target":{"taskId":"t-42"},"payload":{"fields":{"command":"stop"}}}
```

`receive(delivery) -> ack(operationId) | retry | reject(code)`. Ack only
after durable acceptance or identical replay; a crash before ack may
redeliver. An invalid envelope/unknown kind/key conflict is a permanent
reject (dead-letter on real transport); unavailable store means retry. A
later domain failure does not undo the ack. `ack(operationId)` is the adapter
port result, not a required broker RPC reply. A producer can retrieve its
receipt via `GET /operations/by-key/{urlEncodedKey}` -> same shape as the
operation read, 404 if absent. Use a fake delivery source for the R1 adapter
contract. R2b ships an AMQP 0-9-1 adapter with manual ack, durable input and
dead-letter queues and persistent deliveries; queue names are configuration.
An envelope delivering `Start` is **not** a BPMN message catch.

## Release 2: clients and integration

### R2a - inspect, correct and recover

| Route | Input -> output / precondition |
| --- | --- |
| `GET /instances` | Optional `processKey,version,status` filters -> page of instance representations without `variables` or `incident` detail. |
| `GET /instances/{instanceId}/history` | Page of [transition journal](engine.md) entries in ascending revision order. |
| `POST /instances/{instanceId}/variables` (`EditVariables`) | `{expectedRevision,set:{...},unset:[names...]}` -> `{instanceId,revision}`. Reject overlapping keys, duplicate unset names or empty patch; unset of absent key is harmless. Apply atomically to `RUNNING` or `INCIDENT`. Explicit null is a value, not deletion. Never modify a terminal instance. |
| `POST /instances/{instanceId}/retry` (`RetryIncident`) | `{expectedRevision}` -> `{instanceId,revision}`; only in `INCIDENT`. Clear current incident, return to `RUNNING`, requeue failed transition, unfreeze waits (original deadlines). A persistent error creates another incident rather than a retry loop. |

Compare `expectedRevision` **when applying**, not when accepting. Mismatch ->
`REVISION_CONFLICT` with `details.currentRevision`; invalid lifecycle ->
`INVALID_STATE`. Edits and retry each create a revision. No client may silently
overwrite a later edit or automatically resubmit it with a newer revision.

### R2b - correlate one incoming message

Extend the importer with `bpmn:message` (`name`, `zeebe:subscription
correlationKey="= expr"`) referenced by the `messageRef` attribute of a `messageEventDefinition`
on an `intermediateCatchEvent`. One incoming and one outgoing; no message
start, broadcast or message buffering. At entry evaluate a non-empty string
key; store one active subscription unique on `(messageName,correlationKey)`.
An attempted duplicate wait becomes an incident `AMBIGUOUS_SUBSCRIPTION`.

`POST /messages` (`Correlate`) `{messageName,correlationKey,variables}` ->
`{instanceId,revision}`. Match at apply time: consume the single subscription,
merge root writes and resume once in one transaction. No match ->
`NO_SUBSCRIPTION`; a frozen match -> `INSTANCE_IN_INCIDENT` without consuming.
HTTP and AMQP can redeliver the same command with the same key.

## Release 3: migrate a waiting instance

R3 supports a **safe point**, not arbitrary token translation: one `RUNNING`
instance parked at exactly one open user task, no active fork, subscription,
incident, job or other ready token. Its boundary timer may be armed. Target
must be an existing, strictly newer version of the **same** process. Supply
`{expectedRevision,targetVersion,fromElementId,toElementId}`. Match the
current and target user tasks explicitly; no guessing from labels. Target
must have the same set of input keys/types/select values and equivalent timer
presence/duration. Rendering the target view with current variables must
succeed. No variable transforms, bulk or reverse migration in R3.

| Route | Result |
| --- | --- |
| `POST /instances/{instanceId}/migration-preview` | Read-only (no idempotency key, 200); returns `{eligible,checkedRevision,fromVersion,targetVersion,reasons:[{code,message}]}`. Reads one snapshot; not a reservation. |
| `POST /instances/{instanceId}/migrations` (`Migrate`) | Same input. Revalidate revision and eligibility in the commit. Success -> `{instanceId,fromVersion,version,revision,taskId}`. Otherwise `REVISION_CONFLICT` or `INCOMPATIBLE_MIGRATION` with reasons and no changes. |

On success, preserve instance ID, variables, created time and original
absolute deadline. Cancel old task (`MIGRATED`) and timer, create a fresh
activation/task and corresponding timer at the target element, switch the
pin and journal both versions in one commit. Do not run the target start
mapping or reset the deadline. Subsequent steps use only target resources.
Completion of an old task ID cannot complete the replacement. A timer/submit
that won before migration invalidates the checked revision.

## Release 4: external jobs, properties and listeners

**R4a.** Accept `serviceTask` with `zeebe:taskDefinition type="literal"`;
one normal outgoing, no attached boundary timer in this slice. Entering it
parks the token and creates a `READY` job with stable `jobId`, type, input
variable snapshot and three explicit failure attempts. Claim serializes with
other claims, issues a new opaque `claimToken` (fence), and sets expiry.
Use that token to complete/fail. Lease expiry makes the job reclaimable and
invalidates the old claim; expiry is not an explicit failure attempt.

| Route (kind) | Input -> successful operation result |
| --- | --- |
| `POST /jobs/claim` (`ClaimJobs`) | `{type,workerId,limit,leaseMs}` (1..100; 1,000..300,000 ms) -> `{jobs:[{jobId,instanceId,elementId,type,variables,properties,claimToken,expiresAt}]}`, oldest eligible first, possibly empty. |
| `POST /jobs/{jobId}/complete` (`CompleteJob`) | `{claimToken,variables}` -> `{jobId,instanceId,revision}`. Merge root outputs and advance exactly once; late/foreign claim -> `STALE_CLAIM`. |
| `POST /jobs/{jobId}/fail` (`FailJob`) | `{claimToken,message}` -> `{jobId,remainingAttempts,instanceId,revision}`. Decrement once; if attempts remain return job to ready, otherwise incident `JOB_FAILED`. Operator retry resets budget and re-arms that job, not a duplicate activity. |

Workers must tolerate redelivery: use stable `jobId` to deduplicate any
external side effect. Engine guarantees concern state transitions, not an
exactly-once charge/email in another system. Claim/expiry does not advance a
token or change instance revision; do not issue claims or accept job effects
on a frozen instance.

**R4b.** Import `zeebe:properties/zeebe:property` on a supported activity,
as unique literal string `name/value` pairs; no evaluation. Expose via
`GET /definitions/{processKey}/versions/{version}/elements/{elementId}` ->
`{elementId,type,properties}` and pass the map in jobs (empty by default).
First listener profile: one `zeebe:executionListeners/zeebe:executionListener`
with `eventType="end"` and literal job `type` on a script task. Run script
and journal result **once**, create a listener job and park before taking
the outgoing flow. Listener completion merges its outputs and releases the
successor. Failure/expiry follows R4a; retrying the listener never reruns the
script. Reject unsupported placements/phases. Listeners participate in
execution; R5 observers do not.

## Release 5: observe without steering execution

From R5 activation, append a durable outbox record in the same transaction
as each newly observed change. Instance stream: one event per revision with
`stream=instance`, `streamId=instanceId`, `streamSeq=revision`,
`type=instance.transition` and
`data:{kind,processKey,version,status,elementId?,instanceCreatedAt,taskChanges}`.
`kind` is the journal kind. `taskChanges` is an ordered list (possibly empty)
of `{taskId,elementId,version,status,reason?}`; the per-task version is its
own pin (old and new tasks differ in a migration event).
Runner stream: `runner.state` with availability, last heartbeat and active
claim count; engine stream: `engine.state` with ready-work count, overdue
timer count and store observation time. No variable/form payloads by default.

Runners register a unique boot ID and heartbeat every 10 seconds. Observer
marks one `STALE` after 30 seconds without heartbeat and `ACTIVE` on recovery;
this is **not** the work lease protocol. Sample engine counts every 10 seconds
while the store is reachable; on store outage report staleness/503 rather
than inventing fresh zero counts. Persist these observations independently
of user workflows.

An outbox relay appends each `eventId` to a retained feed at most once
(unique event ID); assign strictly increasing global `offset` in **append
commit order**. Serialize offset assignment, preserve order within each
stream, tolerate relay retries. Readers may redeliver; checkpoint last fully
applied offset. Do not page by uncommitted sequence allocation or timestamp.
Engine progress never waits for an observer acknowledgement.

| Query | Inputs -> response |
| --- | --- |
| `GET /events` | `afterOffset` (exclusive, default 0), `limit` (1..200) -> `{events:[{offset,eventId,stream,streamId,streamSeq,type,occurredAt,data}],nextOffset}` in offset order; empty page echoes offset. Retain feed and allow replay from 0 in this release. |
| `GET /runners` | Page of `{runnerId,state,lastHeartbeat,activeClaims}`. |
| `GET /engine` | `{observedAt,readyWork,overdueTimers}` from latest sample; 503 if none. Dashboard shows the age of a sample. |

## Release 6: analytics read model

Project events behind a separate read port; advance projection offset in the
same transaction as aggregates. Replay into an empty projection must give the
same answer. Report coverage beginning at R5 observation activation, not
fictional pre-upgrade history. `coverageFrom`, `asOfOffset` and UTC bounds
are mandatory on responses. Filter `[from,to)` by **event occurrence**, not
ingest time. Late events can change an earlier bucket at a higher offset.

| Query | Input | Result (plus coverage, offset, bounds) |
| --- | --- | --- |
| `GET /analytics/instances` | `from,to,processKey?,version?,bucket=day` | UTC daily rows `{day,started,completed,incidentsRaised,meanDurationMs}`. Count start/completion/incident on their own transitions and active pin at that event. Duration is completion time minus instance creation, for completions in that bucket; null mean when none. Migration is not another start. |
| `GET /analytics/tasks` | `from,to,processKey?,version?` | Rows by `(processKey,version,elementId)` with `{opened,completed,timedOut,p95WaitMs}`. Count open/complete/`TIMER` cancellation by occurrence. Wait sample = close minus open for completions/timeouts with an observed open; exclude migration cancellations. Nearest-rank p95; null for no samples. |

Return rows in ascending bucket/group key order; fill empty days with zeros
and null mean. Reject invalid/reversed bounds or requests starting before
coverage (`400 OUTSIDE_COVERAGE` for the latter). Compute/read projections
without locking the runner hot path. Additional metrics need an event source,
denominator, time basis and replay test before being advertised.

## UI sketches and client parity

R1 is headless. R2a builds the following with HTTP only. Example CLI verbs and
MCP tool names below are thin adapters; inputs/results mirror the route,
including `expectedRevision`, paging, idempotency keys and operation failures.
MCP stands for Model Context Protocol. UI rendering is not a separate
authority for validation or execution.

```text
Instance browser                  Instance i-4    v1  RUNNING  r12
key [Collect...] status [All v]   History: ... -> opened Activity_1hidco5
[i-4 | v1 | RUNNING]              Variables: { "executionCount": 2, ... }
                                  [Edit values] [Save against r12]
                                  [Retry incident] (only when in incident)

Task inbox                        t-9 | due 14:01Z
[t-9 | i-4 | OPEN]                Caller says: hello     Execution: 2
                                  Select [Continue v]    text [        ]
                                  [Done] -> pending -> succeeded / error

Migrate i-4 (R3)                  v1 / Activity_A -> v2 / Activity_B
[Preview] eligible at r12         [Apply against r12]  New task: t-10

Observe (R5)                      Analyze (R6)
Ready 8 | Overdue 1 | age 4s      [UTC from..to] [key] [version]
Runner B: STALE  last 14:05Z      Completed 12 | mean 73s | task p95 58s
Events after #125 [Refresh]       Coverage since ... | through offset 125
```

| Interaction | API backing it | CLI / MCP examples (R2a core) |
| --- | --- | --- |
| Find and inspect instance, history | `GET /instances`, `/instances/{id}`, `/instances/{id}/history` | `instances list/show/history` / `list_instances`, `get_instance`, `get_history` |
| Edit variables, retry incident | `POST .../variables`, `.../retry`; poll operation | `instances edit/retry` / `edit_variables`, `retry_incident` |
| Inbox, render, submit | `GET /tasks`, `/tasks/{id}`; `POST .../complete`; poll operation | `tasks list/show/complete` / `list_tasks`, `get_task`, `complete_task` |
| Check command result | `GET /operations/{id}` | `operations show` / `get_operation` |
| Migrate; observe; analyze | R3 preview/apply routes; R5 runner/engine/feed reads; R6 aggregate reads | No UI-only route, query or write; these surfaces can be built independently of the engine. |

While a command is pending show its operation ID. On a stale task or
revision conflict, show the failure and reload, preserving an unsaved edit
for comparison. Do not show "saved" or navigate away as if 202 meant success.
