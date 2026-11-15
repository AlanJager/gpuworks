# ADR 0066: Agent state is grouped by owner and lifetime, and the composition root only wires

Date: 2026-11-15 · Status: Accepted

## Context

`internal/agent/app` is one package of 89 files and 15.2k lines (non-test); `NodeRuntime` has 75 top-level fields, 11 mutexes and 234 methods, and `OnRPC` is one switch of 50 cases. Images, model files, card health and doctor all hang off this struct and share its locks: a change to any feature changes it, and no subsystem can be tested or replaced on its own.

`internal/agent/engine` keeps an instance's state in 12 parallel maps keyed by UUID, and each exit path deletes some of them. The four that describe the engine process (sleep level, prewarm, L1 host buffer, processes on its cards) are cleared only when the container exits by itself, not when the server removes or stops the instance. Measured (GWKS-321): a new container under the same UUID was reported holding the old process's L1 host buffer, so the server skipped its host memory check; the new process was not prewarmed either.

The server's controller was already reorganized this way: one self-locking type per kind of memory (GWKS-304), and a test that makes every field say whether it is dropped per deployment (GWKS-303). This ADR makes that the agent's pattern.

## Decision

1. **Group by owner and lifetime.** State under one key (an instance UUID, a model file, an image reference, …) that ends at one moment goes into one record (a `struct`), held by one `map[key]*record`, instead of a parallel map per fact. An instance's state is split into records by lifetime: the desired generation, one reconcile attempt (in-flight, pending, cancel), and one engine process (`processFacts`).
2. **Drop the whole record at its one end.** Each kind of record is deleted in one place: the engine process's record where the container is confirmed gone (`reclaimInstance`) and on a runtime switch, never piecemeal on each exit path. A new fact goes into the record it belongs to and is cleared with it.
3. **Every field is classified.** `TestEveryEngineFieldIsClassified` (the same pattern as the controller's `TestEveryControllerFieldIsClassified`) lists each `Engine` field: per-instance ones say when they are dropped; the others say whether they are a dependency, configuration, a lock or a global counter. A new field fails the test until it is classified.
4. **Subsystems own their state and locks, behind narrow dependencies.** Images, model files, card health and doctor each become a package or type holding their own fields and locks, given only the interfaces they need (driver, reporter, storage guard, …), testable from a zero value or a constructor. `NodeRuntime` is the composition root: one field per subsystem, plus the connection and lifecycle wiring.
5. **RPC is a table, not a switch.** `OnRPC` becomes a "method name → handler" table whose handlers the subsystems provide; `NodeRuntime` only looks them up and handles the shared errors.
6. **A ratchet keeps it so.** `agent_node_runtime_fields`, `agent_node_runtime_mutexes`, `agent_onrpc_cases` and `agent_engine_struct_maps` in `scripts/arch-constraints.txt` only go down (initially 75 / 11 / 50 / 9); loosening one needs a `# raised:` reason.

## Consequences

- First step (GWKS-321): the engine process's four maps become `processes map[string]*processFacts`, dropped whole in `reclaimInstance`; `Engine` map fields go 12 → 9; a new container under the same UUID no longer inherits the old process's facts (`TestANewContainerUnderTheSameUUIDKnowsNothingOfTheOldProcess`, for both removal and stop).
- The subsystems are then extracted one at a time (images → model files → card health and doctor → the RPC table), each merged on its own, tightening the ratchet as it goes.

## Costs and what is not done

- `runtimeEpoch` is kept per UUID for the agent process's life on purpose: a removal moves it to a fresh global value, so a reused UUID never matches an old attempt. The cost is one `uint64` per UUID seen.
- The desired-generation and reconcile-attempt maps (`desired`, `inFlight`, `inFlightCancel`, `pending`, `cleanupRequired`) are not merged in this step: they are maintained together by reconcile's serialization protocol, and merging them means changing that protocol with them, which is a change of its own.
- Splitting subsystems does not change the agent–server protocol or behaviour.
