This repository contains the R1 specifications, model fixtures, versioned
database migrations and Go workflow-engine implementation.

# Implementing a work package

Use this guide to carry out one package. The linked specifications define
engine behavior; do not copy their rules into this file.

1. **Find the package.** Locate its ID in the [delivery guide](docs/delivery.md).
   Check that its prerequisites exist in the repository. Note the deliverable
   and the named tests required for handoff. If a prerequisite is missing,
   report it rather than silently taking on another package.
2. **Read the relevant contract.** Use the [architecture](docs/engine.md) for
   ownership and transaction boundaries, the [model profile](docs/model-profile.md)
   for import and execution rules, and the [interfaces](docs/interfaces.md) for
   caller-visible behavior. If two contracts disagree, cite both passages and
   ask for a decision before implementing the disputed behavior.
   Implement the engine in Go. For persistence changes, start with the
   [GORM models and versioned migrations](db/README.md).
   A change is complete only when both backends enforce the same invariant
   and the corresponding contract test passes on both. Do not edit a shipped
   migration or its model snapshot; append a version that upgrades both drivers.
3. **Implement the tests first.** Start by implementing a test that will verify the work is done and have it fail initially.
4. **Implement only the slice.** Work in the owning component and add tests for
   the package's observable outcomes. Keep the [Release 1 fixture](acceptance/release1/)
   unchanged; use a separate test model if a case needs different inputs. Do modify MDs to satisfy exit criteria. Do not modify the tests to finish the work
4. **Verify and hand off.** Run the package's named tests on the backends its
   gate requires. Report the package ID, what changed, the commands run, and
   the outcome per test and backend. Mark anything not run and say why; an
   unavailable backend is not a pass.

For example, when assigned **B4 (boundary timers)**, start at the B4 row in
the delivery guide. Follow the [timer element rule](docs/model-profile.md#bpmn-token-rules)
and the [transition ordering contract](docs/engine.md#observable-transitions-and-ordering).
Use **N3** to exercise the completion race, then check the rest of B4's handoff
tests. Parallel joins belong to C1, not B4.
