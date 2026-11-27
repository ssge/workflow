# workflow

A durable, API-first workflow engine in Go, with SQLite and PostgreSQL persistence.

**Run all native end-to-end (e2e) and integration tests:**

```sh
make e2e
```

This runs the R1 HTTP scenarios against the workflow server on random loopback
ports, plus the Go tests and schema checks, on disposable local databases. No
running server, database URL, or Camunda installation is needed. Prerequisites:
Make, Python 3, Go with CGO and a C compiler, and local PostgreSQL server and
client tools (`pg_config`, `initdb`, `pg_ctl`, `psql`). Run as a non-root user
so `initdb` can start. The timer scenario waits one real minute.

| Read | Use it to answer |
| --- | --- |
| [Engine architecture](docs/engine.md) | What do the terms mean? Who owns state? What must survive a crash? Why these boundaries? |
| [R1 database schema](db/README.md) | How do the Go/GORM models and versioned migrations enforce the same invariants on SQLite and PostgreSQL? |
| [Executable model profile](docs/model-profile.md) | Which BPMN, DMN, form and FEEL constructs must the importer accept, and how do they execute? |
| [Interfaces and UI](docs/interfaces.md) | What does a caller send or receive? What does an operator or task participant see? |
| [Delivery and verification](docs/delivery.md) | Which epic comes next? What can be built independently, and what observable result proves it works? |

See the [Release 1 application](acceptance/release1/) for a worked model and
[AGENTS.md](AGENTS.md) for guidance on implementing and verifying one work package.
