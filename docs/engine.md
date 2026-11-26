# Engine architecture

**Status:** implementation contract. "Must" denotes a release gate. R1 rules
continue to apply unless a later release explicitly extends them. This is a
behavioral specification. The implementation language is Go; GORM is confined
to the persistence adapters. See the [model profile](model-profile.md), [interfaces](interfaces.md)
and [release gates](delivery.md).

## Ubiquitous language

| Term | One meaning in this specification |
| --- | --- |
| Engine | The system that stores and executes process instances. Not a particular process, host or runner. |
| Bundle | Source files submitted together: in R1, one executable BPMN process and all DMN decisions and forms it uses. References do not escape the bundle. |
| Process key / element ID | Respectively the BPMN process `id` (stable across versions) and a model element's `id` within its definition. Neither identifies a running task. |
| Definition / version | One immutable, compiled bundle for a process key; `version` is its positive integer publication number, scoped to that key. |
| Head | The definition chosen for **new instances** of one process key. Changed only by atomic publication. |
| Instance / pin | One execution, identified by `instanceId`, and its exact definition reference `(processKey, version)`. A pin covers decisions and forms too. |
| Drain | Let instances pinned to a former head finish without placing new starts on it. Not suspension, deletion, or migration. |
| Token | Durable control position within an instance. Multiple tokens mean multiple paths of control, not multiple instances. |
| Activation | One visit by a token to an element. A loop creates a new activation even when the element ID is the same. |
| Transition | One atomic change to an instance, including all writes and successor work it causes. Identified by `(instanceId, revision)` after commit. |
| Revision | Monotonically increasing instance state number. Starts at 1. Not the model version, API version or lease fence. |
| Variables | The instance's JSON object of named values. A form submission is input to a transition; it is not a second variable store. |
| User task | A durable request for human input for one activation, identified by `taskId`. "Task" in the task API always means this record, never runner work. |
| Boundary timer | A durable deadline attached to an activation. In R1 it can interrupt that activation once. |
| Command / operation | A request to mutate state / its durable receipt and eventual outcome. Acceptance is not execution. |
| Idempotency key | Caller-generated identity for retrying the **same command**. Not an instance ID, task ID or BPMN message correlation key. |
| Work item / lease / fence | A durable unit eligible for a runner / a time-limited claim / a monotonically increasing claim generation used to reject stale writes. |
| Runner | An interchangeable, stateless executor of leased work. Its death cannot lose a wait, variable, deadline or successor. |
| Job / worker | From R4a, a durable request to external application code / the client that claims it. A worker is not an engine runner. |
| Incident | Recorded inability to execute a validly deployed model with the current instance data. Not a malformed request, storage outage or normal wait. |
| Adapter | Translation at a port (e.g. SQL or test driver). It must not supply missing business semantics for the component it adapts. |

## Components and dependencies

```text
 UI / CLI / MCP / application                  broker delivery (R2b)
              | HTTP                                   |
              +------> I/O service <-------------------+
                        | append command + receipt       reads
                        v                                |
                 durable inbox/work       Query API / User Task API
                        ^                                |
               lease / commit                     owner read ports
                        |
          runner A, B, ... (no instance affinity)
                        |
             application transition coordinator
                 |        |           |          |
              catalog   runtime    user tasks   clock
                 |        |           |
              importer   pure step + FEEL/DMN evaluator
                 \        |           /
                 transaction/repository ports
                            |
             configured SQLite OR PostgreSQL adapter
```

Arrows denote calls, not requirements for separate hosts. Domain logic depends
on values and port contracts, never on HTTP, SQL, a broker, a Camunda client,
a wall clock or a UI. No component reaches into another owner's tables.

| Owner | Inputs -> outputs; boundary |
| --- | --- |
| I/O service | `accept(key, kind, target, payload)` -> receipt or admission error. Check envelope/shape and deduplicate in a short transaction. Never compile a model, evaluate FEEL, select a branch, wait for a runner or advance a token. HTTP and broker adapters share this port. |
| Catalog & importer | `compile(files)` -> validated immutable execution plan and original sources, or diagnostics with file/element/field. `publish(plan, tx)` -> new `(key,version)` and head. `load(ref)` -> that plan only. Caches must be keyed by the full ref; eviction changes no result. |
| Coordinator & runtime | `decide(plan, position, variables, trigger)` -> effects or typed execution error. Apply effects under the unit of work. Own instances, tokens, joins and timers. The pure evaluator never persists or performs external I/O. |
| User Task Service | `open(tx, activation, formView) -> taskId`, `consume(tx, taskId, submission) -> variable writes`, `cancel(tx, taskId, reason)`. Owns task state, form views, validation and public task queries. Public completion goes through ingress, **not** directly to `consume`. The coordinator uses this service inside the same transaction as the token move. |
| Scheduler & runner | `claim(owner, limit, leaseDuration) -> [(workId,fence,expiresAt)]`; `apply(claim)` -> committed, obsolete, retryable conflict or incident. Select ready work, including due timers; no business routing in the scheduler. Polling is sufficient; a notification is only a wake-up hint. |
| Store & clock | `withTransaction(fn)`, conditional writes, uniqueness, ordered keyset reads, authoritative UTC time. Bind every owner port in a transition to the same transaction. Store adapter translates contention into retryable conflicts. Clock is replaceable for tests, shared by all runners and the scheduler. |
| Query service | Reads committed owner projections -> public representations. R1 reads need no event bus, analytics system or separate eventually consistent index. Must not start or repair execution as a side effect of a read. |

### Minimum persistent facts

The table below defines the logical facts. The [R1 schema and migration index](../db/README.md)
links the shared [GORM models](../db/models.go) and [versioned migrations](../db/migrate.go)
that enforce them on SQLite and PostgreSQL. Atomicity and ordering still require the execution
protocol below; persist data before acknowledging it.

| Record | Identity, facts and important lookup/constraint |
| --- | --- |
| Definition, head | Unique `(processKey,version)`; sources, pinned resource IDs, compiled plan/profile revision. One head per key. Concurrent successful publications allocate distinct consecutive versions in commit order. No hard delete in these releases. |
| Operation/inbox | Unique global idempotency key and unique `operationId`; normalized request, status, result or error. Keep outcomes and keys through R1-R6 (no expiry promised). A pending operation has recoverable work. |
| Instance & transition journal | Pin, status, revision, variables, created/ended times. For every changed revision append exactly one entry `(instanceId,revision,kind,elementId?,at,fromVersion?,toVersion?)`; no entry for a rejected command. Use `STARTED` at creation, `COMPLETED` at terminal end, `INCIDENT_RAISED` on failure, `MIGRATED` on repin, `VARIABLES_EDITED` / `INCIDENT_RETRIED` for the two manual repairs, `STEP` for other transitions. Journal is not an event bus or the source for rebuilding current state. |
| Token, fork, join arrival | Instance, position, activation identity, fork occurrence and branch identity where applicable. Unique arrival per `(forkId,incomingFlowId)`; a closed fork cannot release again. |
| User task, timer | Unique task per user-task activation. Timer references activation, not just element ID; includes `dueAt` and `ARMED/FIRED/CANCELLED`. Index open tasks and ready deadlines. Retain terminal task status. |
| Work item | Kind, reference, ready time, claim owner/fence/expiry, state. Index by eligibility; successor creation is in the predecessor commit. At most one logical work item per cause (e.g. operation ID, ready token visit or timer ID). |
| Incident | Instance, failed work/position, element, code, human-readable reason, revision. Retain enough input to retry the **transition**, not partially executed effects. |

Startup configuration selects `database.driver` (`sqlite` or `postgres`) and
`database.connection` (file path or PostgreSQL connection string). Initialize
or validate the schema before accepting traffic; fail startup on an unknown
driver, unavailable store or incompatible schema. Never silently fall back
to an empty local store. All runners and services of one installation must
point at the same database; SQLite means one shared local file, not one file
per runner.

## Execution protocol

1. **Admission.** Insert operation and runnable command work together. A
   transport acknowledges only after this commit. A retry after a lost
   acknowledgement finds the same operation. Admission ordering is not an
   execution-order promise.
2. **Claim.** In a short transaction claim only eligible items (`readyAt <=
   now`, unclaimed or expired) by compare-and-set, increment fence, commit.
   Use finite leases and bounded batches; repeatedly scan commands, token
   steps and timers so a busy class cannot starve the others. Do not hold a
   DB lock while waiting for input or a deadline.
3. **Apply.** In one transaction check current fence, owner, unexpired lease
   and current domain preconditions; serialize competing updates to an
   instance (lock, or compare revision and retry from fresh data). Recheck
   ownership on commit via a fenced conditional update; zero affected rows
   means discard the **whole** attempt. Apply one command or one element
   transition, not a polling loop over the diagram. Persist all effects,
   journal/revision where applicable, mark this work done, enqueue successors
   and finish any associated operation together. A successor is claimable by
   **any** runner; there is no in-memory continuation.
4. **Recovery.** Crash before commit -> no effects; expiry makes the work
   reclaimable. Crash after commit -> durable effects and no second advance.
   A storage failure or serialization conflict rolls back; retry with backoff
   (or let the lease expire), never convert it to model success or an incident.
   A stale timer/obsolete internal wake-up is consumed without changing the
   instance. A new client command that loses a race gets an explicit outcome,
   not silent success.

### Observable transitions and ordering

| Trigger and precondition | Single commit (success) |
| --- | --- |
| Publish valid bundle | Move head and store immutable resources; finish deploy operation. Invalid bundle changes neither head nor version history. |
| Start by key | Serialize head selection with publication. At **instance creation commit**, pin current head, copy initial variables, set `RUNNING`, revision 1, enqueue start token. No head -> failed operation. No public start-by-version. |
| Ready token at automatic element | Evaluate against current committed variables; write results, consume position, schedule next position(s). No external side effects. See [element rules](model-profile.md). |
| Enter user task | Open a fresh task with pinned form view, park token, arm any deadline relative to activation time, atomically. `RUNNING` includes parked waits. |
| Submit open task | Validate fields; mark `COMPLETED`, merge permitted writes, cancel its armed timer, schedule the normal outgoing path. `operation SUCCEEDED` does not mean the process has ended. |
| Fire due timer on open task | Mark timer `FIRED`, cancel task (`reason=TIMER`), resume **only** via the boundary outgoing flow. No form values are submitted. |
| Reach end | Consume token. Set `COMPLETED` and `endedAt` only if no live token, join arrival awaiting siblings, or wait remains. Terminal instances are read-only. |
| Deterministic step error | Roll back effects of step; in a fenced commit against the same revision record incident, block its work, set `INCIDENT`, increment revision. Freeze automatic work, timers and task completions for that instance. R2a can repair variables and retry. Other instances continue. |

The first **committed** interrupt or completion wins, not the first network
request or lease. `dueAt` makes a timer eligible, not a hard cut-off on task
submission. A completion that wins after `dueAt` but before the timer commit
is legal. A command targeting a terminal task fails `TASK_NOT_OPEN`, even if
the loop has opened another task at the same element.

All root variable writes are serialized per instance; no stale whole-object
replacement. Parallel paths read at transition time. Disjoint changes survive;
two increments each see the previous committed value. For conflicting
assignments, last committed write wins; models must not assume branch order.
Runners can work on different instances concurrently, and on different
activations of the same instance over its lifetime. Correctness cannot require
two writers to commit simultaneously.

### Worked exchange: deploy while an instance waits

Deploy v1 and await success; start A and await its open task. Publish v2.
A's next form, script and decision still use v1, even if another runner picks
up its completion. Start B after publication succeeds: it pins v2. A start
queued **before** publication but applied **after** it also pins v2. To
sequence calls, await the preceding operation, not just HTTP 202. A former
head is `DRAINING` while it has `RUNNING` or `INCIDENT` instances and `DRAINED`
after the last one leaves; head status is `CURRENT`. These are derived
definition labels, not commands sent to runners.

## Accepted architecture decisions

| ADR | Decision and reason | Alternative rejected / consequence |
| --- | --- | --- |
| 01 - Durable ingress | Separate acknowledgement from execution; all mutating transports use one durable inbox. Survives runner downtime and redelivery. | Synchronous execution in the HTTP handler couples availability and hides uncertain outcomes. Callers instead poll an operation. |
| 02 - Transactional stepping | Current relational state plus journal; fenced finite leases and per-instance serialization. Exactly one **committed transition** per consumed cause, not exactly-once delivery. | An in-memory actor per instance needs affinity/recovery; event sourcing is unnecessary to meet R1. Work may be attempted more than once. |
| 03 - Logical task service | Separate contract and ownership; in R1 co-locate it with the engine and share the unit of work. | A mandatory network hop/second DB would need a distributed consistency protocol for timer vs submit. A future physical split must preserve the same outcomes. |
| 04 - Replaceable store | Choose one adapter at startup; run the same contract and acceptance suite against SQLite and PostgreSQL. SQLite may serialize writers; PostgreSQL may use row locks. | No dependency on `SKIP LOCKED`, advisory locks, JSON query dialects or broker ordering in the domain. No live cutover or conversion of existing data. |
| 05 - Pin the whole bundle | Publish atomically, bind at start, never look up "latest" mid-instance. R3 migration is a separate explicit command. | Independent late binding of a form or decision would change a draining instance without an auditable migration. |
| 06 - Profile, not emulation | Accept a named, tested subset; reject unsupported execution semantics at deployment. Portable tests describe outcomes, not engine internals. | Neither full BPMN/DMN coverage nor Camunda client/wire compatibility is claimed. Existing parsers/evaluators are replaceable behind the import/evaluation ports. |
| 07 - API first | UI, CLI and MCP use the same application APIs as other clients. No authentication or authorization in these releases. | No client-only mutation, direct SQL admin tools or privileged MCP bypass. Run only in a trusted environment until a security model is added. |
| 08 - Internal transport | Component boundaries do not imply network boundaries. R1 requires no internal RPC protocol: ingress hands work to runners through the durable inbox, and task completion shares a transaction with timer cancellation and token movement. | gRPC is optional, not a prerequisite. Any future transport must preserve the same transaction and recovery guarantees. |
| 09 - Go and GORM | Implement the engine in Go. Define the relational schema once in GORM, with ordered migrations tested on both drivers. Keep GORM models out of domain and HTTP contracts. | Do not maintain parallel SQL schemas or run unrestricted `AutoMigrate` on startup. Use dialect-specific migration code only for invariants GORM cannot express portably. |
