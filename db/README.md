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

## Verification

From the repository root:

```sh
go test ./...
python3 db/test_schema.py sqlite
python3 db/test_schema.py postgres
```

The [shared checks](test_schema.py) run the Go migrator, exercise its constraints
independently of GORM, then rerun it against the populated store. They reopen a
temporary SQLite file or create a private, Unix-socket-only PostgreSQL cluster. The latter requires a non-root
user, `pg_config`, and its matching `initdb`, `pg_ctl` and `psql` binaries.
Neither command uses an application database. These tests check DDL and
rollback; they do **not** replace the [adapter and runner gates](../docs/delivery.md#component-storage-and-native-gates).
