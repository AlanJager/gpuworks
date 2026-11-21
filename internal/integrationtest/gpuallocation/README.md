# GPU allocation business tests

Run from the repository root:

```sh
go test -count=1 -timeout 10m ./internal/integrationtest/gpuallocation/...
go test -race -count=1 -timeout 40m ./internal/integrationtest/gpuallocation/...
```

No GPU, GPU driver, CUDA installation, container daemon, model download or
remote machine is required. Go dependencies must already be available for an
offline run. The lifecycle package starts the real server and agent on local
ports, with `testenv.FakeProber`, `testenv.FakeDriver` and a local model fixture.
The fake named `docker` advertises runtime capabilities; it does not execute
Docker. SQLite databases and local engine HTTP listeners are temporary.

The lifecycle contention cases can each take about a minute: other deployments
retry on a periodic node report after a claim is released. Their polling budget
allows two report periods, the settle backoff and runner slack; there are no
fixed sleeps in these cases.
The suite is included by `go test ./...` and `make test-integration`.

## Organization

```text
gpuallocation/
  scheduler/          Requests + snapshots -> plans, refusals
    schedule_test.go       Local case types, assertions, selection matrix
    combinations_test.go   Full product of scheduler facts, independent expectations
    topology_test.go       Topology x capacity x pinning
    parallel_matrix_test.go TP/PP/DP/node shape x memory x pin width
  planning/           Persisted facts -> claims, snapshots, quota room
    ledger_test.go         Claim kind x instance state; sleep/wake sequences
    quota_test.go          Selected replicas x foreign occupancy x new limit
    quota_nodes_test.go    Node identity x selected replicas x co-tenant state
    wake_matrix_test.go    Atomic/partial wakes and replica-local rollback
    group_matrix_test.go   Group constraints x strategy x actual card trial
  deployment/         Public Evaluate/Create -> preview and persisted intent
    admission_test.go      Placement mode x allocation mode x occupancy x policy
    pool_matrix_test.go    Strategy x node eligibility x replica shape
  lifecycle/          Real API/controller/agent with fake hardware/runtime
    contention_test.go     Concurrent creates, cleanup failure, release/retry
    pinned_replicas_test.go Migrated pinned-replica regression
    reconnect_test.go      Claim conservation across disconnect and adoption
  internal/fixtures/  Store/node/model setup and shared contract budgets
  COVERAGE.md         Allocation rules -> expectations -> owners -> named cases
  coverage_test.go    Guard against stale case/source references in the catalog
```

There is no common `CaseRunner`, action language or replacement allocator.
Case, row and expectation types are private to their test packages. Production
interfaces stay unchanged. Fixtures create facts; `budget.go` restates the few
contract budgets several packages expect (the NVIDIA slice claim, the foreign
holder and its headroom) with their source. Nothing here asks the allocator
what it should decide. The architecture gate excludes only the fixtures package
from the production store-import budget.

Shared fixtures keep complete node reports: foreign holders enter through
`UpdateNodeStatus`, and an operator's decision about a card uses `SetCardScheduling`.
Group trials use an empty store so synthetic nodes cannot inherit another
fixture node's claims or topology.

## Coverage catalog

The [rule coverage catalog](COVERAGE.md) is the only inventory of rules, their
expected behavior, implementation owners and named cases. Existing focused
tests stay in their owning packages, such as the candidate matrix in
`internal/server/scheduler`, and the catalog links them in place. Only the
pinned-replica case moved here, from `integrationtest/deploy`. The catalog
states behavior and interaction claims, not complete branch or
Cartesian-product coverage.

## Combining dimensions without multiplying every expensive test

1. **Focused cases** pin down boundaries and exact refusal codes at their
   owning layer, with literal expected results. In the node-pool matrix both
   nodes fit, and each eligibility gate excludes the otherwise preferred node.
   Every gate must change the choice and appear as the exact candidate refusal;
   ranking or insufficient capacity cannot stand in for that evidence.
2. **Scheduler combinations** check the full product of nine finite
   dimensions against an independent expectation. `Schedule` is a pure
   function, so checking every combination costs less than sampling them.
   Each allocation mode must also have an accepted and a refused row with
   every eligibility gate open, so an unrelated refusal is never its only
   negative evidence. Consent, policy and sleep refusals are checked with
   their exact codes while capacity fits.
3. **Stateful and expensive paths** use explicit business sequences and small
   exhaustive matrices over persisted facts. The real agent/server contention
   test has two allocation modes, not the entire scheduler product.

The scheduler input model tests facts accepted by `NodeSnapshot`, including a
synthetic `SharedByMemory` card. It does not claim that every combination is a
physically supported vendor/runtime configuration. Hardware-derived capability
and protocol restrictions have their own planning/agent tests. If a new input
constraint makes some combinations invalid, leave them out of
`scheduleCombinationInputs` with the reason, instead of silently dropping
failing cases.

Expected results are not obtained by calling `GPU.UsableFor`, `CapacityFor`,
`Claim`, `QuotaRoom` or the estimator a second time. The combination oracle
uses explicit fixture budgets and rules; boundary tables contain literal
expected results. This still requires review: an oracle that repeats a
production mistake is a bad test even when every combination is checked.

## Adding a dimension or rule

For a new card gate, for example:

1. Add focused allow/refuse and boundary cases in the owning package, such as
   `internal/server/scheduler/gpu_candidates_test.go`, or in
   `schedule_test.go`, with the expected refusal code.
2. Add a named dimension and its finite values in `combinations_test.go`, map it
   to the real request/snapshot in `input`, and update the independent
   `expectation`. Every combination, old and new, is then checked. If the new
   value closes an eligibility gate, include it in `eligibilityGatesOpen` so
   each mode keeps a witness that is refused for capacity.
3. If the product becomes slow enough to matter, give the dimension its own
   focused interaction instead of sampling the product.
4. If the fact comes from persistence or a request field, add the corresponding
   planning or deployment matrix row to prove propagation. Add a lifecycle
   sequence only when ordering, contention or recovery is part of the rule.
5. Update the owning row and affected interactions in [COVERAGE.md](COVERAGE.md).
   Its reference test checks that the linked case functions still exist; run
   the actual linked cases as well as the focused suite and relevant checks.
   Implementation-owner labels are descriptive: their file paths, not their
   Go symbol names, are checked.
   A discovered bug becomes a named regression with an explicit expected
   result, rather than only a random seed.

Keep fixtures local until two packages genuinely share setup. Avoid exporting
production internals for tests. Review positive cases, boundary cases, stable
failure reasons and state transitions along with combination coverage. No
statement coverage percentage or combination claim proves arbitrary
higher-order sequences, vendor enforcement, GPU memory isolation, CUDA
correctness or real NCCL communication; those need their respective tests.
