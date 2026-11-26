# Delivery guide and acceptance specification

Read across a row for the epics that deliver a usable release. Read down a
column to follow one participant's work over time. A dash means no new epic,
not removal of existing behavior. The contracts are in [engine](engine.md),
[models](model-profile.md) and [interfaces](interfaces.md). No implementation
or test run is implied by this map.

## Epic story map

| Release / outcome | Modeler: publish & evolve | Participant / integrator: start & act | Operator: inspect & correct | Platform: execute & recover | Analyst: observe & measure |
| --- | --- | --- | --- | --- | --- |
| **1a - publish and run a straight path** | Validated bundle; immutable versions | HTTP deploy, start, receipts | Read pin and outcome | Durable inbox; fenced runners; SQLite + PostgreSQL | - |
| **1b - run the supplied application** | FEEL, DMN FIRST, form binding, XOR/loop | Task API and resolved forms; automatic timeout | See task, count and final decision | Durable human waits and interrupting timer race | - |
| **1c - release the distributed engine** | Structured AND; publish successor bundle | Both branches actionable; new starts on head | See old versions drain | Concurrent/restarted runners; portability and DB matrix gates | - |
| **2a - self-service operations** | - | Human inbox UI | UI, CLI, MCP: inspect, edit and retry via APIs | CAS on revisions; history and incident recovery | - |
| **2b - integrate a message producer** | Intermediate message catch by name/key | **Concrete AMQP adapter** and **BPMN message correlation** | Query receipt from producer key | Durable subscription, redelivery and dead-letter handling | - |
| **3 - upgrade a waiting instance** | Define compatible element mapping | Receive replacement task on target version | Preview/apply guarded migration | Atomic repin; retain variables and absolute deadline | - |
| **4a - delegate external work** | Service task with literal job type | **External service workers** claim/complete/fail | Diagnose exhausted jobs | Leased, fenced jobs and retry budget | - |
| **4b - extend activity behavior** | Literal extension properties and end listener | Listener handler uses worker API | Read properties and listener failure | Block exit until listener completion | - |
| **5 - observe live state** | - | Read-only event consumer | Runner/engine/instance dashboard | Heartbeats, transactional outbox and replayable feed | Reliable event inputs |
| **6 - analyze history** | - | - | Compare process versions and task waits | Rebuildable projection independent of execution | Throughput, cycle time, incidents, timeouts and wait percentiles |

## R1 implementation packages

Take one row at a time. Prerequisites are **package IDs**, not permission to
implement an entire subsequent release. The interim 1a/1b gates allow a
narrower model profile; 1c must satisfy the complete R1 profile. A temporary
in-memory fake is useful for pure tests, never a substitute for either
persistent adapter at a release gate.

The [R1 database schema](../db/README.md) maps each storage package to a
versioned Go migration. Use the shared GORM models rather than inferring tables
from HTTP representations. Run the [shared schema checks](../db/test_schema.py)
on both backends in addition to the adapter/runner tests named below.

| ID / gate | Needs | Implement this boundary; output | Proof before handoff |
| --- | --- | --- | --- |
| A1 / 1a | - | Value types, pure FEEL subset, errors and shared fake-clock/UTC interface. No SQL or HTTP. | Unit vectors U1. |
| A2 / 1a | - | Store contracts, migrations and SQLite adapter for inbox, head, instance, journal and work. | S1-S2 against a reopened file, not `:memory:` per connection. |
| A3 / 1a | A2 contracts | PostgreSQL adapter; startup selects one store for all components. Same migrations' logical constraints and transaction behavior. | Exact same S1-S2 on a real PostgreSQL database. |
| A4 / 1a | A1 | Import start -> end bundles to a validated plan, diagnose invalid graph, serialize/deserialize plan without changing IDs. | U2 with minimal positive/negative bundles. |
| A5 / 1a | A2, A4 | Catalog publish, command coordinator, leased runner, start/none-end transitions and journal; I/O port only appends. | N1 (through ports), N2 (straight path), version allocation in N5. |
| A6 / 1a | A5 | HTTP command/query/receipt adapters (definition and instance reads) and fake broker delivery contract. | N1 through transports; deploy/start/await a completed instance with runners independent of ingress. Repeat on A3. |
| B1 / 1b | A1, A4 | Add BPMN mappings/script/XOR, DMN parser and table evaluator, immutable resource binding. | U1-U3 including FIRST precedence, missing inputs and no route. |
| B2 / 1b | A1, A4 | Parse and validate schema-19 forms; pure render and submission validator. | U4; no dependency on task UI or SQL. |
| B3 / 1b | A5, A6, B1, B2 | Task service open/submit/query, parked token, atomic normal exit; extend store adapters and task HTTP port. | Generated timer-free single-task model: submit -> field writes and end. Invalid fields leave task open; duplicate submission cannot advance twice. |
| B4 / 1b | B3 | Persist, claim and consume due timers; race against task completion with one transaction. Now the entire unchanged application can deploy. | P1-P4; N2 recovery, N3 race interleavings and N6 isolation on both stores. |
| C1 / 1c | B4 | Structured parallel compiler, fork occurrence, durable join arrivals; both store adapters. | P5 and N4, including re-entry. |
| C2 / 1c | B4 | Drain/version reads under concurrent publications and starts; prove sibling resources stay pinned. | P6 and N5. |
| C3 / 1c | A6; B4/C1/C2 for full suite | Native and Camunda test drivers, isolated test data, portable scenario runner, capability report. Can start the driver contract before the engine exists. | Portable scenarios and native matrix below; no SQL in portable assertions. |

For every row: deliver the named port/behavior, its tests, and any migration
for **both** stores when adding state. A package is not done if it changes
published wire shapes, silently broadens the profile, or requires the same
runner to pick up the next step. Keep changes inside the owning component;
add a port method only when a named scenario needs it.

## Verification architecture

Separate **P** (public model behavior), **U** (pure component),
**S** (store contract), **N** (native engine/transport) and **L** (later release)
suites. Only the native harness can stop a runner, control leases, inject a
transaction fault or inspect an internal work item. Do not require a Camunda
deployment to expose our schema, operation API, journal, revision or clock.

Portable scenario interface (language-neutral):

```text
deploy(files)                              -> DefinitionHandle
start(processKey, variables)               -> InstanceHandle
awaitOpenTasks(instance, elementId?, n)    -> [TaskHandle]
readForm(task)                             -> view              [form-view capability]
submit(task, fields)                       -> accepted | no-longer-open
awaitInstance(instance, predicate)         -> {state, variables, definition}
waitDeadline(task)                         -> void              [timer capability]
```

Handles are opaque and scoped to a driver. `deploy` and `start` return after
their command succeeds; `submit` waits for the command outcome, not the
completion of all successor work. Queries may poll a public history/query
service. `waitDeadline` controls a supported test clock or waits bounded real
time; it must **not** simulate the timer transition by submitting a task.
Use bounded eventual assertions (native virtual time: pump eligible work to
quiescence; external: configurable deadline greater than 60s plus query lag).
Report last observed state, handles and timeout on failure.

* **Native driver:** calls only the public API for P assertions; harness may
  supply clock/control outside that API. Run the *same* P and N scenarios on
  SQLite (two independent connections to one file) and PostgreSQL (two
  independent connections to one database). No production clock-advance or
  fault-injection endpoint.
* **Reference driver:** implement translation to a configured Camunda 8
  deployment's public deployment, instance, task and query/history APIs.
  Submit the exact fixture bytes; normalize only IDs, version handles,
  terminal/wait state names, task field representation and asynchronous timing.
  Never evaluate the decision or emulate an unsupported feature in the driver.
  Use a fresh namespace/environment per suite or unique new process keys for
  generated models; run the fixed-key fixture in isolation.
* **Report:** per backend+version, scenario and capability, record PASS,
  FAIL (including fixture rejection/semantic divergence), UNSUPPORTED
  (optional assertion not exposed), or NOT RUN (environment missing).
  Neither of the latter two is a pass. The R1 gate requires a green native
  matrix and a real reference comparison of deployment and observable
  outcomes; document differences, don't weaken assertions to match either
  engine. Never label Camunda wire compatibility as an outcome. Nonportable
  policies (fencing, replay via our key, late binding of reference-engine
  resources, resolved form view availability) are not shared guarantees.

## R1 test catalog

### Portable scenarios: what a user can observe

For P1-P4 deploy the [release1 directory](../acceptance/release1/) as supplied.
Use `{input:"hello"}` on start. Bind every task handle from the latest query,
not from a BPMN element ID. Wait for each **new** task handle after a loop;
this avoids mistaking a lagging query result for another activation.

| ID | Given / when | Then |
| --- | --- | --- |
| P1 - immediate exit | First open task, submit `text_command="terminate"`. Repeat with a new instance using `command="terminate"` instead. | Before submission count is 0; if form view exposed: `Caller says: hello`, `Execution: 0`, select default `continue`, Unicode textfield label preserved. Each instance ends at 1, `decision="break"`, no open task. |
| P2 - FIRST beats stop | Submit `command="stop"` on successive activations. | First and second submissions reopen with count 1 then 2 and `decision="loop"`; third completes at 3, `decision="break"`. Repeat using text field `text_command="stop"` and select `continue`. |
| P3 - maximum loop count | Always submit `command="continue"` without letting a timer expire. | New activations for counts 1..10, then completed at 11 with `decision="break"`; exactly 11 submissions. |
| P4 - interrupting timer | Start with `command="continue",text_command=""` in addition to `input`. Leave first task alone until its deadline. After replacement task appears, attempt submission on the old handle and terminate via the new one. | Without any first submission, count becomes 1 and a distinct task opens. Old handle cannot advance; new task completes instance at 2. Native timer tests below also cover absent input keys. |
| P5 - parallel join | Generated bundle: start -> AND split -> one form task per branch (`left`, `right`, disjoint input keys) -> AND join -> increment `joined` -> end. Initialize `joined=0`. Complete in either order in fresh instances. | Both tasks open before either completes. After one, instance still running, sibling open and `joined=0`; after both, completed, both field writes retained, `joined=1`. |
| P6 - old process path drains | Deploy a task-wait process v1 whose post-task script writes marker `"old"`; start A and park. Deploy same key v2 writing `"new"`; start B and park. Complete both. | A has original definition and `"old"`; B has new definition and `"new"`. Same form/decision sources for this **portable** check. N5 separately exercises native bundle pinning. |

### Component, storage and native gates

| ID | Arrange and assert |
| --- | --- |
| U1 - FEEL | `0.1 + 0.2` -> exact `0.3`; string concatenation in fixture -> expected text; `decision = "loop"` -> Boolean; missing name -> null; `"1" + 1` -> type error. Reject function/syntax outside profile at compile time. |
| U2 - import | Accept a none start/end model and the unchanged full fixture at 1b; reject dangling target, duplicate IDs, missing decision/form, unsupported node or non-interrupting timer. Report file and offending field/element; changing namespace prefixes only must not alter plan behavior. Test XOR with two true flows (first wins), false with default, false without default (incident). |
| U3 - decision | Table-driven evaluation of actual DMN: `(count,command,text_command)` -> scalar: `(1,stop,null)` -> loop; `(1,null,terminate)` -> break; `(3,stop,null)` -> break; `(10,continue,null)` -> loop; `(11,continue,null)` -> break. Test separate no-match table -> null. |
| U4 - form | Render the actual form at count 2; verify order, texts, Unicode and default. Submit invalid select/unknown key -> field errors, omitted key -> no write. Re-render from a different binding and verify no cached cross-instance value. |
| S1 - durable unit of work | Commit instance change plus successor then reopen: both present. Inject failure between writes and reopen: neither present. Two concurrent inserts for one operation key yield one record. Verify constraints and scoped version uniqueness on both adapters. |
| S2 - claim & time | Two claimants on one item -> one live claim. Frozen time before expiry: no reclaim; at expiry: new higher fence. Old fence cannot commit journal, work or variable changes, even if that runner resumes. Future timer not claimable. |
| N1 - inbox & replay | Stop runners. Submit deploy through HTTP -> 202, pending durable receipt, no head. Redeliver identical key via fake broker -> ack same ID; different body -> reject conflict. Restart runners -> one version. Simulate crash after inbox commit/before ack, and store unavailable before commit: respectively dedup on replay and retry/no ack. Invalid deploy operation leaves head unchanged. |
| N2 - runner handoff & crash | Pump a straight path one committed item at a time on alternating runners; rerun on fixture including a wait. Kill A before commit, advance lease clock, let B finish; separately kill A after commit/before next poll. No lost step, doubled increment, stuck task, duplicate journal revision or affinity. Restart **all** runners over a saved open task/deadline; no work before due time, and due work progresses without a new message. |
| N3 - task vs timer | Fresh instances with absent command fields: at 59,999ms first task still open; at 60,000ms eligible. Schedule both possible winners: submit `terminate` first -> completed at 1, timer cancelled; timer first -> cancelled task, one increment, replacement task, late submit `TASK_NOT_OPEN`. Force both runners to contend at commit. Same-key completion replay returns original result; different key after close fails. Invalid fields on an open task leave status, deadline and variables untouched. |
| N4 - parallel integrity | For P5 submit branches concurrently with different runners; crash on last arrival and replay. Exactly one successor, no orphan wait. Wrap whole region in a two-iteration loop; no join mixing between visits/instances. Replace both branch bodies with scripts incrementing same counter from 0 -> final 2. Reject cross-branch or bypass-join edges at deployment. |
| N5 - version semantics | Race two valid publishes (distinct consecutive versions), then invalid publish (no head change). Pause a Start before apply, publish, resume: pin at creation. On a fresh fixture, park A on v1; publish v2 with same form/decision IDs but changed display and decision rule. A's **next visit** still renders v1 and follows v1 rule; new B renders/uses v2. Old version `DRAINING` until A ends, then `DRAINED`. |
| N6 - incident isolation | Generate script `= amount + 1`, start with a string `amount`; instance in `INCIDENT`, element/code retained, no result write or successor. Other instances still complete. Restart runners: incident is not auto-retried. |

For gates 1a/1b, run the applicable prefix of each named scenario; 1c requires
**all** U/S/N/P cases, including N6. Run native time/race gates with a shared
controllable clock and barriers or failpoints at claim/commit, not probabilistic
sleeps. For a timer-winning scenario, assert state **before** advancing time
again so the loop's next timer cannot obscure a double advance.

## Later work packages and release gates

Each row is independently assignable after its prerequisites; all passed R1
tests remain regression gates. Read the matching release section in
[interfaces](interfaces.md) for request, response and failure shapes.

| Package | Needs / deliverable | Acceptance gate |
| --- | --- | --- |
| 2A - read & repair | R1c -> instance listing/history, revision-checked patch, incident retry. | **L1:** two edits with same revision -> one success, other conflict; no lost write. Repair `amount` in N6 and retry -> one result, completion, unchanged old journal entries. Reject terminal edit. |
| 2B - clients | 2A -> operator UI/CLI/MCP plus task inbox over public APIs. | **L2:** run inspect/edit/read-back via each client against same API; API read shows exact value. Repeat stale edit, surface conflict. UI task submission and API-only submission yield same P1 outcome. No DB credentials in clients. |
| 2C - real broker | R1c inbox contract -> deployable AMQP adapter. | **L3:** kill consumer after persistence/before ack; redelivery resolves to same operation. Invalid envelope dead-letters; transient DB error redelivers. HTTP lookup by key returns outcome. |
| 2D - correlation | R1c compiler/waits -> message catch and Correlate API; pair with 2C for R2b gate. | **L4:** wrong key fails/no advance; matching key via HTTP then redelivery via AMQP produces one resume/merge. Same pair cannot create two active subscriptions. No pre-wait buffering. |
| 3A - eligibility | R2a read/repair + R2b waits -> read-only migration preview. | **L5:** compatible task reports eligible with actual revision; incompatible fields/timer, older version, active join or subscription reports reason, never changes state. |
| 3B - commit | 3A -> atomic migration command, history and UI preview/apply. | **L6:** migrate a 60s task at +45s. New task/pin, same variables, old handle invalid, timeout still at +60s. Preview then submit old task before apply -> conflict, no half-migration. |
| 4A - worker protocol | R1c step protocol + 2A retry -> service-task import, job creation, worker routes. | **L7:** let claim expire and reclaim; late completion fails, new completion advances once. Lost response + same key replays result. Three explicit failures -> incident; operator retry re-arms same job. |
| 4B - extensions | 4A -> property import/read and blocking end-listener hook. | **L8:** listener sees literal properties and committed script result; downstream cannot start before completion. Fail then retry listener: script counter incremented once. Reject unsupported hook placements. |
| 5A - observations | Existing transition owners through R4 -> outbox, heartbeats, engine samples, feed and relay. | **L9:** kill relay between append attempts; replay has one logical event per ID, ascending stream sequence, no committed offsets skipped. Dead runner becomes stale without stopping instance recovery. No subscriber required to complete P1. |
| 5B - dashboard | 5A -> state queries and read-only UI. | **L10:** stop one runner, show last heartbeat and stale status; concurrent progress by another appears in instance feed. Reconnect after saved offset: retrieve unseen events. |
| 6A - process analytics | 5A -> projection checkpoint and daily process report. | **L11:** in one UTC bucket, two starts and two completions of durations 2s and 6s -> counts 2/2 and mean 4,000ms. A completion at exclusive `to` is excluded. Replay/redelivery leaves identical aggregates. |
| 6B - task analytics | 6A -> task group report and filterable UI. | **L12:** one completed task after 10s and one timeout after 60s, same group -> opened 2, completed 1, timedOut 1, p95 60,000ms. Pre-coverage input rejected; replay and fresh projection agree through the same offset. |

**First implementation handoff:** A1, A2 and the portable driver interface
can start without an engine. The first runnable demonstration is A6: stop
runners, accept a start for a published definition, restart a different
runner, and read the completed instance. Add the real form/decision/timer
fixture next; do not begin a UI to prove that the engine works.
