# workflow

Implementation specification for a durable, API-first workflow engine in Go.
The R1 database schema and migrator use GORM; the engine itself is not yet runnable.

| Read | Use it to answer |
| --- | --- |
| [Engine architecture](docs/engine.md) | What do the terms mean? Who owns state? What must survive a crash? Why these boundaries? |
| [R1 database schema](db/README.md) | How do the Go/GORM models and versioned migrations enforce the same invariants on SQLite and PostgreSQL? |
| [Executable model profile](docs/model-profile.md) | Which BPMN, DMN, form and FEEL constructs must the importer accept, and how do they execute? |
| [Interfaces and UI](docs/interfaces.md) | What does a caller send or receive? What does an operator or task participant see? |
| [Delivery and verification](docs/delivery.md) | Which epic comes next? What can be built independently, and what observable result proves it works? |

See the [Release 1 application](acceptance/release1/) for a worked model and
[AGENTS.md](AGENTS.md) for guidance on implementing and verifying one work package.
