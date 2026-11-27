# R1 database schema

The [Go/GORM models](models.go) define the tables, relationships, constraints
and indexes for both backends. The [versioned migrator](migrate.go) creates
them in four stages. [Column types](types.go) handle dialect differences in
JSON, command keys and the timer-only foreign key. [Database guards](guards.go)
use trigger DDL where GORM has no equivalent; callbacks would not protect
writes made outside the ORM. There is no second schema in SQL files.
The [engine contract](../docs/engine.md#execution-protocol) defines the
transactions that use these internal tables.

| Version | Gate / package | Adds |
| --- | --- | --- |
| 1 | 1a / A2, A3 | Catalog, heads, inbox, instances, token visits, journal, leased work and incidents |
| 2 | 1b / B3 | Human tasks and saved form views |
| 3 | 1b / B4 | Boundary timers and the timer work reference |
| 4 | 1c / C1 | Fork occurrences, branch identity and join arrivals |

## Applying migrations

Run the [migrator](../cmd/workflow-migrate/main.go) once per installation before
starting runners. Set `WORKFLOW_DATABASE_CONNECTION` to a file path for
`sqlite` or to a DSN for `postgres`. For example:

```sh
WORKFLOW_DATABASE_CONNECTION=./workflow.db go run ./cmd/workflow-migrate -driver sqlite
```

The command uses `db.Open` and `db.Migrate`, applies only the missing suffix,
and commits each version and its ledger row in the same transaction. Existing
R1 databases retain their `schema_migrations` numbers and data. A gap, unknown
version, or disagreement between the ledger and engine tables is an error, not
permission to recreate tables. After release, preserve model definitions used
by shipped migrations and append a new migration. Do not use `AutoMigrate`
as a substitute for an explicit upgrade.

SQLite requires `STRICT` table support and the JSON functions (SQLite 3.38+
with JSON enabled); building the Go SQLite driver requires CGO and a C compiler.
`db.Open` configures **every connection**, not just the migrator, to enable
`PRAGMA foreign_keys = ON` and `PRAGMA recursive_triggers = ON` before starting
a transaction. Do not use `INSERT OR REPLACE`: it deletes and reinserts rows.
For PostgreSQL use a UTF-8 database and a consistent schema/search path.

## Reading and writing the rows

- Times are signed 64-bit UTC Unix milliseconds from the store/clock port.
  JSON is stored as text in SQLite and `json` in PostgreSQL. Bind complete JSON
  documents without passing numbers through binary floats. Validate duplicate
  keys and decimal limits at ingress/import; a JSON column cannot do that for
  the engine. Compare normalized commands by value, not serialized bytes.
- A definition stores the original `[{name,content}]` plus a compiled plan
  containing its resource-ID bindings. A head and an instance pin both reference
  a **pair** `(process_key, version)`. R1 task versions are read through the
  activation's immutable instance pin; task components are saved, not rendered
  again. Drain labels are derived, not stored.
- `activations` records one visit per row and at most one live position for a
  token. On movement, consume the old visit and insert the new visit in the
  same transaction. A command work item is keyed by operation; step and timer
  work are keyed by kind and activation. R1 allows one timer per task activation.
  Create its work with `ready_at = due_at`. Keep terminal rows so old handles
  and delayed notifications cannot refer to a later visit.
- Compare the current fence, owner and lease expiry in the final conditional
  work update; if it affects zero rows, roll back the **whole** transaction.
  Likewise, publish version and head together, write one journal entry with
  each revision change, and resolve a task/timer race in one transaction.
  For a join, check the arrivals against the pinned plan and conditionally
  close the open fork before scheduling its single successor. Uniqueness
  prevents duplicates; it does not perform any of these transitions.

## A2 runtime port

After `Open` and `Migrate`, use `db.NewRuntimeStore(database, clock)` with a
shared UTC millisecond clock. [The `db/store` package](store/store.go) contains
the interfaces and values; callers do not need GORM models. Inputs are
already validated and normalized by their owners. Operations and work IDs
are supplied by callers.

- `Store.Admit` atomically inserts a pending receipt and command work, or
  replays the original receipt. Request equality ignores JSON object order
  and compares numbers exactly by decimal value; a changed command returns
  `ErrIdempotencyConflict`.
- `Store.WithTransaction` binds the catalog, instance, journal, activation
  and step-work methods to one transaction. `Tx.Publish` allocates a version
  and advances the head; `Tx.CreateInstanceAtHead` serializes with publication
  and writes revision 1 with `STARTED`. `Tx.ChangeInstance` compares the
  expected revision and appends the next journal entry without changing the pin.
- `Tx.ListDefinitions` pages version metadata in ascending order using an
  exclusive version cursor (zero for the first page). `Tx.GetHead` reads the
  current ref without reserving it. `Tx.ListInstancePins` returns instance
  IDs and statuses for an exact ref; `Tx.CountInstancePins` optionally
  filters by caller-selected statuses. These reads return facts, not drain
  labels. PostgreSQL uses `READ COMMITTED`, so separate reads in a callback
  can see intervening commits.
- `Tx.ListUserTasks` returns ordered task records joined to their activation
  and pinned instance, with the original absolute deadline when present.
  Task queries are read-only; resolved form components remain the saved view
  on both open and terminal tasks.
- `Store.Claim` takes up to the requested limit for one work kind, at or
  after `ReadyAt` and, on reclaim, at or after lease expiry, increasing each
  item's fence.
  `Store.Apply` checks the persisted owner, fence and expiry before the
  callback and in the final conditional update. It commits effects, command
  outcome if required, and work completion together. `ErrLeaseLost` means
  no effects committed. On `ErrConflict`, retry from fresh state, not from
  previously computed effects.

Task, timer and fork methods on `Tx` persist the caller's effects. Task
closure compares `OPEN`; timer transitions compare `ARMED`. `EnqueueTimerWork`
uses the persisted deadline. Cancellation leaves queued work intact: an
already leased or later due wake-up can be consumed without effects after
the caller reads the cancelled state. Recording a join arrival locks the fork
against concurrent arrivals and closure for the rest of the transaction.
The caller checks join readiness against the plan before closing the fork.

After rolling back a failed step, `Store.RaiseIncident` uses the same fence
checks as `Apply` in a separate transaction. It compares the failed revision,
writes the next `INCIDENT_RAISED` revision and incident, and marks the leased
STEP or TIMER work `BLOCKED` instead of `DONE`. Claims exclude automatic work
for instances in `INCIDENT`, preserving ready steps and deadlines while an
incident is open. Obsolete work on terminal instances can be consumed without
changing them. Task completion checks the same lifecycle state.
Incident repair and retry are outside R1.

## Verification

From the repository root:

```sh
go test ./...
WORKFLOW_TEST_POSTGRES_DSN='host=… dbname=… user=…' go test -run TestRuntimeStore ./db
python3 db/test_schema.py sqlite
python3 db/test_schema.py postgres
```

`TestRuntimeStore` runs S1–S2 against reopened SQLite files and the same suite
against PostgreSQL when the DSN is set. Use a disposable PostgreSQL database
with permission to create and drop test schemas. Without it, that backend
is skipped.

The [shared checks](test_schema.py) run the Go migrator, exercise its constraints
independently of GORM, then rerun it against the populated store. They reopen a
temporary SQLite file or create a private, Unix-socket-only PostgreSQL cluster. The latter requires a non-root
user, `pg_config`, and its matching `initdb`, `pg_ctl` and `psql` binaries.
Neither command uses an application database. These tests check DDL and
rollback; they do **not** replace the [adapter and runner gates](../docs/delivery.md#component-storage-and-native-gates).
