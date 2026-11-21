# ADR 0080: The Cluster Engine Is Compiled into the Server Through a Runtime Interface, No Longer Split by a Build Tag

Date: 2026-11-21 · Status: Accepted, not yet implemented (adds to [ADR 0070](0070-extension-tiers-and-edition-boundary.md) decision 2; leaves [ADR 0071](0071-installable-extensions.md) decision 18 unchanged)

## Background

The control plane cluster engine lives in the open-source repository, kept out of the default build by the `dqlite` build tag. The community release does not compile it in; a server with the cluster runtime is built with `-tags dqlite,nosqlite3`, and the cluster commands and the database helper are in the enterprise repository.

In one week this tag took the number of build-tagged production files from 69 to 265. Measured on `52f24211a`:

- 132 production files carry `linux && dqlite` (`app` 98, `store` 17, `cluster` 16, `testenv` 1), and 15 stub files carry `!linux || !dqlite` to give the same symbols a single-node implementation;
- only 13 of the 132 files really import `github.com/canonical/go-dqlite/v3` (`cluster` 10, `store` 2, `app` 1); the rest carry the tag because they use a tagged symbol;
- the stubs declare 42 top-level symbols, of which only 14 have a caller in the default build; the other 28 exist only for the enterprise repository's cluster commands;
- the tagged code adds 53 methods to `*App` and 10 to `*Store`, and uses 37 unexported identifiers of `app` and 33 of `store`;
- there are 185 more tagged test files. Open-source CI only runs `vet` on the tagged build and does not run these tests.

The tag is not the engine's boundary either. The whole `internal/server/enrollment` package, about 51 cluster files in `store`, and the migrations of the cluster tables carry no tag; the default build compiles them and has no use for them. So the runtime fact "is this server a cluster member" is written as pairs of files: changing one interface means changing the tagged implementation and its stub, and open-source CI can test only the stub half.

`app` in fact already has a runtime interface: `haRuntime` (`ha_runtime.go`, 8 methods, `nil` on a single node). What the tag decides is the part outside it: who opens the database, and who constructs this runtime.

`nosqlite3` is a different matter: without it, go-dqlite's `client` package imports `github.com/mattn/go-sqlite3`, and this repository uses only the glebarez driver. It is a switch for the whole build, not something written on files.

## Decision

1. **The engine stays in the open-source repository, in a package of its own, and is handed to the server as a value.** A separately built server passes the cluster runtime to `app.Config` through `cli.Options`; the zero value is single-node SQLite, identical line by line to today's `cmd/gpuworks`. `cmd/gpuworks` does not import the engine package, and the community binary does not link go-dqlite. This extends ADR 0070 decision 2 (a single entry point, the zero value is the community edition) to the database layer.
2. **The `dqlite` build tag is removed from production code, and the 15 stub files are deleted.** Platform tags (`linux`) remain only on files that really call Linux system interfaces. "Is it a member" is answered by the runtime passed in, not by the compiler.
3. **The interface takes the three existing seams and invents no new concept.**
   - `app`: the existing `haRuntime`, plus the three entry points the tag selects today: opening a member whose business is closed (`openClosedMember`), opening the business store (`openServeStore`), and observing the cluster (`observeCluster`).
   - `store`: the five functions that open the runtime database and the three replication error predicates (`ReplicatedReadInterrupted`, `ReplicatedLeaderUnavailable`, `ReplicatedWriterBusy`) become one backend value given when the Store is opened; the default is the SQLite backend, for which the three predicates are always false. The cluster callback fields the Store already has (local scheduling leader, membership grant, transport authorization and so on) are given with the backend.
   - `cluster`: the two constructors specific to go-dqlite (`StartDatabase`, `OpenRemoteExistingReadSQL`).
4. **The runtime gets a narrow host, not `*App`.** The methods of `haRuntime` take `*App` today and read its unexported fields directly. Once the engine leaves the package, those reads converge on one interface that `app` implements. It is composition inside the core, not an extension point on `app.Host`: ADR 0070 decision 5 (extensions get neither the Store nor a database handle) continues to hold, because the engine is core code in the same repository, refactored and tested together with the core.
5. **`nosqlite3` is given once in the Makefile as a build switch**, and no longer appears paired with `dqlite` in targets. The architecture check gains one rule: the dependency closure of `cmd/gpuworks` contains neither go-dqlite nor `mattn/go-sqlite3`.
6. **The tagged tests become ordinary tests of the engine package.** Cases that need the real helper skip and print the reason when `GPUWORKS_DQLITE_BIN` is absent, and fail under `GPUWORKS_TEST_STRICT=1`. The existing tests of the core packages (`store`, `api`, `app`) must be able to run once more against the cluster backend, with the backend chosen by a runtime switch in `testenv` that replaces today's `defaultDatabase` tag pair.
7. **The community release still has no cluster feature**; the cluster commands, the helper, packaging and the entry layer stay in the enterprise repository, and whether it is licensed is still undecided. This ADR changes only how the engine gets into the server.

## Implementation Order

Each step is merged on its own; after it, both the default build and the build with the cluster runtime compile and pass their tests. The enterprise repository's cluster commands depend on exported functions of `app` and `cluster`, so every step that changes their signatures comes with a change in the enterprise repository and an update of the open-source commit it pins.

1. `store`: a backend value replaces the pair `runtime_sqlite.go` and `runtime_dqlite.go`, as well as `replicated_read_error_*`, `import_runtime_*`, and `database_*` in `testenv`.
2. `app`: the three entry points are passed in through `Config`, and the five `app` stubs are deleted.
3. `cluster`: the two constructors are passed in through the same value, and the six `cluster` stubs are deleted.
4. Move the `app` files that only the cluster uses into the engine package, with the methods of `haRuntime` taking the narrow host; remove the tag from those files.
5. `cli.Options` gains the cluster runtime; the enterprise repository passes the engine in instead of adding the tag; add the dependency closure check, and give the number of files carrying the `dqlite` tag a budget that only goes down, ending at 0.

## Trade-offs

- **Moving everything to the enterprise repository (not adopted).** The unpackaging work is the same as in this plan, and in addition the open-source repository would have to keep about 130 to 190 exported symbols stable for good, including the opening of the database and the Store callbacks, which is exactly what ADR 0070 decision 5 rules out; every refactor on the write path becomes a change in two repositories. The migrations of the cluster tables and about 5500 lines of cluster code in `store` stay in the open-source repository regardless, so open source would not be left with only an interface. Enterprise CI today re-runs about 960 test files of `store`, `api`, `app` and others under the tag, and that is the only place that verifies that core behaviour also holds on the cluster backend; with the engine outside the repository, these in-package tests can no longer run against it.
- **Moving only `cluster` and the files that import go-dqlite (not adopted, possible later).** This is the only way to take go-dqlite out of the open-source `go.mod`, and the exported surface to keep stable is small (about 31 `cluster` symbols and 3 predicates). But it leaves about 115 tagged glue files in the open-source repository that open-source CI can then never run. After this plan the boundary is clear; if a licensing policy then requires go-dqlite to leave the open-source repository, the cost of this step is moving one package.
- **The cost of compiling the engine into the default build.** The open-source repository's `go test ./...` compiles the engine package; cases that need the helper skip without it, so the skip count has to be read as `CLAUDE.md` requires, and a skip must not be taken for a pass. The engine package needs `app` and `store` to export a set of internal interfaces, a wider surface than today's same-package access, but one that can be refactored atomically inside one repository.

## Consequences

- Changing a cluster-related interface changes one place; the compiler of the default build sees all of its implementations.
- When adding a capability that "exists only in one deployment shape", first ask whether it can be expressed through the value passed in; build tags are only for files that really cannot compile on another platform.
- In the enterprise repository `gpuworks-ee`, ADR 0017 decision 1 ("the engine is behind the `dqlite` build tag") is revised with step 5 to "the engine is passed in through `cli.Options`"; its other decisions are unchanged.

## Implementation Status

- Not started. The figures above are static measurements on `52f24211a`; whether the tagged test suite passed at that time was not verified.
- The concrete shape of the interface (the method set, the fields of the narrow host) is settled in the merge requests of steps 1 and 2, and written back to this page once settled.

<!-- translation-source-sha256: 6db21607e17fa38b78596912b0d2daeb44a7617751aba64f0b94bd9b9d0e1360 -->
