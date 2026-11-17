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
includes that production cadence; there are no fixed sleeps in these cases.
The suite is included by `go test ./...` and `make test-integration`.

## Organization

```text
gpuallocation/
  scheduler/          Requests + snapshots -> candidates, plans, refusals
    candidates_test.go     Migrated resource and boundary matrices
    schedule_test.go       Local case types, assertions, selection matrix
    combinations_test.go   Dimensions, pair coverage, independent expectations
    topology_test.go       Topology x capacity x pinning
  planning/           Persisted facts -> claims, snapshots, quota room
    ledger_test.go         Claim kind x instance state; sleep/wake sequences
    quota_test.go          Selected replicas x foreign occupancy x new limit
  deployment/         Public Evaluate/Create -> preview and persisted intent
    admission_test.go      Placement mode x allocation mode x occupancy x policy
  lifecycle/          Real API/controller/agent with fake hardware/runtime
    contention_test.go     Concurrent creates, cleanup failure, release/retry
    pinned_replicas_test.go Migrated pinned-replica regression
  internal/fixtures/  Store/node/model setup shared by planning and deployment
```

There is no common `CaseRunner`, action language or replacement allocator.
`scheduleInput`, `scheduleCase`, `scheduleExpectation` and
`allocationExpectation` are private to their test packages. Production
interfaces stay unchanged. Fixtures create facts; they do not calculate what
the allocator should decide. The architecture gate excludes this test tree's
importable helpers from production engine-literal and store-import budgets.

## Coverage matrix

These are behavior/interaction claims, not a claim of complete branch or
Cartesian-product coverage. Existing focused unit tests remain authoritative
for the details listed in the final column.

| Responsibility | Central matrix / sequence | Assertions | Additional existing coverage |
| --- | --- | --- | --- |
| Candidate eligibility | Whole/dGPU, NVIDIA/Ascend backend, occupancy, schedulability; exact fit/one byte short, unavailable index/backend, foreign holders, degradation | Availability, stable refusal, candidate list independent of chosen pins/model fit | [Scheduler feature and contract rules](../../server/scheduler/scheduler_test.go) |
| Memory admission | Whole/partition/native sharing x occupancy x sleep residue x automatic/operator/platform selection | Admission/refusal, chosen card, group shape, no input mutation | [Backend reserves and contracts](../../server/scheduler/scheduler_test.go) |
| Foreign sharing | Known/unknown occupancy, backend, health, policy, consent and sleep among nine factors; full consent x policy x sleep interaction with ample capacity | Manual consent works, platform pins do not consent, sleep refusal wins | [Agent protocol and measured occupancy](../../server/planning/foreign_share_test.go), [consent propagation](../../server/planning/foreign_consent_test.go) |
| GPU group selection | Sparse/unknown/busy pins, healthy/degraded alternatives, TP/head divisibility, PP; NVLink x capacity x pin | Exact selected indexes and TP, eligible topology fallback | [TP/PP/DP](../../server/scheduler/parallel_test.go), [automatic DP](../../server/scheduler/dp_automatic_test.go) |
| Node prerequisites | Dtype, serving network, model disk, runtime disk | Stable refusal codes | [Target capabilities](../../server/scheduler/target_capabilities_test.go) |
| Claim ledger | Whole/partition/native claim x pending/pulling/starting/optimizing/running/draining/pending-removal/removing/failed-sleep/failed-launch/stopped | Exact allocated bytes, whole-card marker, instance membership, snapshot agreement, other card untouched | [Yield reservations, exclusions and candidate projections](../../server/planning/gpu_candidates_test.go) |
| Sleep/wake accounting | Each claim kind: awake -> sleep ordered -> sleep reported -> wake ordered -> awake reported -> stopped | Full claim until confirmation, measured residue, full reservation during wake, final release | [Residue estimates and concurrent wakes](../../server/planning/residue_test.go), [multi-node atomic/partial wakes](../../server/planning/multinode_sleep_test.go) |
| Quota changes | One/all selected replicas x absent/known foreign holder x shrink/grow/overflow | Every selected claim counted, sibling claim retained, exact shortfall, preview does not mutate quota | [Unknown foreign memory](../../server/planning/quota_room_foreign_test.go), [API live updates](../../server/api/deployments_dgpu_quota_test.go) |
| Deployment entry | Auto/manual/auto with echoed cards x whole/partition x free/whole/shared/foreign occupancy x policy | Preview semantics, selected node, create acceptance/refusal, stored quota/pins, no rows on refused create | [Generated Evaluate/Create properties](../../server/deployment/property_evaluate_create_test.go) |
| Preview/create race | Claims inserted after automatic pick, before revalidation | Conflict; losing create persists nothing | [Deployment selection properties](../../server/deployment/property_selection_test.go) |
| Runtime lifecycle | Three concurrent intents for one card, whole and partition modes; injected removal failure then recovery | Capacity never exceeded at checkpoints, failed cleanup retains claims, waiter eventually runs after release | [Other deployment lifecycles](../deploy/), [distributed workflows](../deploy/distributed_test.go) |
| Replica pinning | Scale a pinned deployment while a different card is free | Desired replicas preserved, no fallback outside pins, busy reason and event | Case moved here from `integrationtest/deploy`, not copied |
| Fleet ranking | Covered in existing packages; not yet a central cross-matrix dimension | Spread/binpack, candidate-pool filtering and trial order | [Pool matrix](../../server/planning/pool_test.go), [target pool](../../server/planning/target_pool_regression_test.go), [controller placement](../../server/controller/unit_pool_test.go) |

Links above are relative to this directory. Existing cases were moved only when
they depended solely on public APIs and had no private helper dependencies.
The candidate matrix and pinned-replica case have one authoritative location.

## Combining dimensions without multiplying every expensive test

1. **Focused matrices** pin down boundaries and exact expectations at their
   owning layer. They are not replaced by pairwise generation.
2. **Cheap scheduler combinations** have nine finite dimensions. Their 2,592
   synthetic fact combinations produce a deterministic 14-row pairwise set,
   covering all 214 value pairs. A separate assertion enumerates the expected
   pairs directly from the dimension domains and fails if any pair is missing.
   Row count is informational, not a compatibility requirement.
3. **Critical interactions** fix irrelevant gates open and exhaust the relevant
   dimensions: 54 mode/occupancy/residue/selection rows and 12
   consent/policy/sleep rows. Each allocation mode must have positive and
   negative witnesses. Pairwise coverage alone does not guarantee those.
4. **Stateful and expensive paths** use explicit business sequences and small
   exhaustive matrices. The service matrix has 48 rows. The real agent/server
   contention test has two allocation modes, not the entire scheduler product.

The scheduler input model tests facts accepted by `NodeSnapshot`, including a
synthetic `SharedByMemory` card. It does not claim that every combination is a
physically supported vendor/runtime configuration. Hardware-derived capability
and protocol restrictions have their own planning/agent tests. If a new input
constraint is introduced, enumerate valid rows first and compute reachable
pairs from those rows; document unreachable pairs instead of silently dropping
failing cases. If enumeration becomes costly, check in a constraint-aware
covering array and retain a coverage check.

Expected results are not obtained by calling `GPU.UsableFor`, `CapacityFor`,
`Claim` or `QuotaRoom` a second time. The combination oracle uses explicit
fixture budgets and rules; boundary tables contain literal expected results.
This still requires review: an oracle that repeats a production mistake is a
bad test even when every pair is covered.

## Adding a dimension or rule

For a new card gate, for example:

1. Add focused allow/refuse and boundary cases in `candidates_test.go` or
   `schedule_test.go`, with the expected refusal code.
2. Add a named dimension and its finite values in `combinations_test.go`, map it
   to the real request/snapshot in `input`, and update the independent
   `expectation`. Existing pairwise rows regenerate automatically. Review new
   constraints and both successful and refused witnesses.
3. Fully combine it with the few existing dimensions that jointly decide the
   new rule. Do not promote every dimension to full-product coverage.
4. If the fact comes from persistence or a request field, add the corresponding
   planning or deployment matrix row to prove propagation. Add a lifecycle
   sequence only when ordering, contention or recovery is part of the rule.
5. Update this coverage table and run the focused suite, race detector and the
   repository's normal checks. A discovered bug becomes a named regression
   with an explicit expected result, rather than only a random seed.

Keep fixtures local until two packages genuinely share setup. Avoid exporting
production internals for tests. Review positive cases, boundary cases, stable
failure reasons and state transitions along with pair coverage. No statement
coverage percentage or two-way coverage claim proves arbitrary higher-order
combinations, vendor enforcement, GPU memory isolation, CUDA correctness or
real NCCL communication; those need their respective tests.
