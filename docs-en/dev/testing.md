# Testing Guide

[`CLAUDE.md`](../../CLAUDE.md) is the sole authority for testing requirements. This guide explains how to select and run tests at each layer;
it does not redefine quality gates.

Real-browser console tests follow the [Browser E2E Testing Standard](../../test/e2e/browser-e2e-testing-standard.md): inventory shared components and
business domains, and build an evidence chain using real requests, persistence, agent execution, and applicable runtimes. `make test-ui` and
`npm run test:coverage` run unit/RTL tests and do not demonstrate browser coverage of business workflows. See that standard for existing Python Playwright entry points,
fixture boundaries, and browser gates that still need to be integrated.

> **Do not run L3 on a machine with an online `gpuworks-agent`.** L3 fixtures start a test agent on the same docker daemon. An agent's orphan cleanup deletes containers with the `gpuworks-*` name prefix that are outside its own desired set, destroying instances managed by the production agent (this happened on gpu3 on 2026-09-16; see T9 in `todo.md`). To run L3 on such a machine, stop the production agent first and restart it after the tests finish.

## L1–L4

| Layer | Environment | Purpose |
|---|---|---|
| L1 (simulation) | Single process, fakes/fixtures | Pure logic, protocols, errors, and edge cases; offline and reproducible by default |
| L2 (multi-process) | Multiple real local processes | Server/agent reconnection, streams, process exit, and recovery |
| L3 (container) | Local Docker/udocker/nerdctl | Container networking, mounts, runtime CLIs, and fake engines |
| L4 (cloud GPU) | Explicitly selected physical machines or cloud resources | Acceptance testing for GPUs/drivers/vendor runtimes, real downloads, and inference |

```bash
make test
make test-integration
make test-coverage-report
```

Before committing, also run `go test -race` for relevant packages, perform compile-only checks (compile without executing) for test suites with explicit build tags, and run
`make vet` and `git diff --check`. Only ordinary tests may use `t.Skip` when external capabilities are unavailable;
with `GPUWORKS_TEST_STRICT=1`, missing critical dependencies must cause a failure.

## Load Tests (Outside the Default Suite)

These tests measure elapsed time (wall-clock time) and resource consumption. They are excluded from the default test suite and explicitly enabled with `GPUWORKS_TEST_PERF=1`.
Here, fleet means a test cluster running agents in bulk; churn means repeated cycles of deployment, image pulling, entering running, and deletion.
The two multi-process tests referenced by CI have Make targets. Run the others directly with `go test`. Full commands, including `-tags`, `-timeout`, and scaling variables, are in the comments of each test function:

| Entry Point | Test | What It Measures |
|---|---|---|
| `go test` (see the `skipUnlessPerf` comment) | `TestLanes_*` in `internal/integrationtest/tunnel` | Dual-lane throughput and ctrl latency |
| `go test` (see the test comments) | `TestFleet1000NodesRedeployChurn` | Control-plane write pressure during large-scale node onboarding and repeated deployments (single process) |
| `make test-fleet-perf-processes` | `TestFleet1000NodesRedeployChurnAcrossProcesses` | The same load, with the server in a separate process and the fleet in another process |
| `go test` (see the test comments) | `TestFleetLogCenterScale` | Log center read targets under fleet load (single process) |
| `make test-fleet-log-scale-processes` | `TestFleetLogCenterScaleAcrossProcesses` | The same log center load, with the server and fleet in separate processes |
| `go test` (see the test comments) | `TestFleetGatewayLoadAcrossProcesses` | `/v1` data plane: server process + multi-GPU fleet (streaming fake engines) + streaming clients. Reports req/s, TTFT/E2E, server CPU core usage and memory, and replica distribution. Use `GPUWORKS_GATEWAY_*` to adjust the workload configuration; see the baseline in `artifacts/archive/2026-09-24-gateway-load-acceptance/gateway-load-gpu2-2026-09-24/` |

Fleet tests use 1000 agents by default, half of which repeatedly cycle through deployment → image pulling → running → deletion. The database writes to disk
to measure the real cost of writes; `:memory:` does not perform fsync (synchronizing data to disk), so it cannot measure this overhead. The tests assert that:

- Online nodes never go more than 60 s without a heartbeat update (heartbeats queued behind progress writes are the target failure of this remediation).
- No nodes remain offline when churn ends.
- Every deployment enters running.
- Each deployment has ≤ 20 event rows.

The tests also record, without assertions: the number of nodes that disconnect and reconnect during churn, p50/p99 (50th/99th percentile latency) for
`GET /deployments` and the events endpoint, a single `GET /deployments` request's latency when writes are idle, database growth,
RSS (resident process memory), and CPU usage.

### Two Configurations, One Shared Driver

All code from “all nodes online” to “churn complete” lives in `internal/integrationtest/fleet`
(REST client, node liveness observation, churn loops, reporting, and verdicts). Both configurations share this code; only the process topology differs:

- **Single-process configuration** (`internal/integrationtest/fleet`, outside the default suite): the server and 1000 agents run in
  one test process. This is the simplest setup, but its RSS/CPU/scheduler measurements combine both, so it **cannot answer “how much does the control plane itself need?”**
  Models and model files are written directly through the store as setup outside the measurement window.
- **Multi-process configuration** (`test/l2`, `-tags=multiproc`): the real `gpuworks` binary runs in a separate process, while another
  `test/l2/cmd/fleet-host` process hosts N agents (real agentapp + the same fake prober/driver).
  Nodes must obtain tokens through REST (`POST /api/v1/nodes`). Models use the product's own “register an existing directory on a node”
  path (`POST /api/v1/models/local` and `/models/{id}/register-path`), without downloading a single byte.
  RSS and CPU usage for the two processes are read separately from `/proc`.

**The multi-process configuration is the authoritative scale test**: only it separates the server's own overhead from that of 1000 agents,
and its measurements are more stable. The single-process configuration remains available for quick, small-scale trial runs. Neither is part of default CI. In GitLab,
the `l2-fleet-redeploy-churn` job runs the multi-process configuration and appears only in schedule / web / api
pipelines with `FLEET_REDEPLOY_CHURN=1` (the nightly schedule includes this variable). The job runs with `GPUWORKS_TEST_STRICT=1`, so an insufficient file descriptor limit
causes a failure rather than a skip. Logs from both processes are retained as artifacts for 14 days. Disconnect/reconnect counts and endpoint p99 are currently reported without assertions;
thresholds will be set after enough nightly runs have accumulated to establish trends.

The multi-process configuration can run with prebuilt binaries. **This is the only way to run it on machines without the Go toolchain**:

```bash
GPUWORKS_FLEET_SERVER_BIN=/path/gpuworks GPUWORKS_FLEET_HOST_BIN=/path/fleet-host \
GPUWORKS_FLEET_WORK_DIR=/path/work make test-fleet-perf-processes
```

`GPUWORKS_FLEET_WORK_DIR` places the run's database, agent state, and logs in the specified directory (so you can investigate the preserved state
after a failure). `GPUWORKS_FLEET_PROCESS_LOGS=1` separately saves stdout/stderr from both processes. On a machine already running production services,
use a private network namespace to isolate ports (engine port allocation always starts at 30000).
The following is a command skeleton; replace `...` with the actual test command selected in this section:

```bash
unshare -n bash -c 'ip link set lo up; ulimit -n 65536; ...'
```

### Disconnects and Reconnects

Disconnect/reconnect counts are reported without assertions. The diagnostic and fix history for this metric is retained below to explain the current verdict criteria.
Disconnects were previously attributed to “insufficient CPU when one process runs the server and 1000 agents,”
but measurements disproved this explanation: instability persisted on an idle 20-core machine. A race condition existed in the transport layer. `internal/wsconn`
fixed one trigger path, and the keepalive interval adjustment described below fixed the other. The original sequence was:

1. The go-yamux send loop updates the write deadline only after half of the connection write timeout period has elapsed.
2. coder/websocket's `net.Conn` implements write timeouts with a sticky “expired” flag. `SetWriteDeadline`
   clears the flag and reschedules the timer, but cannot revoke a callback already released to run. If the old callback arrives late, it marks the flag expired again after renewal.
3. The next frame fails immediately with `failed to write: context deadline exceeded`, before anything is written to the socket.
   yamux treats a send-loop error as fatal to the entire session. The server then runs `deregister`, and the node becomes offline.

On both ends, `laneKeepAliveInterval` was exactly
half of `laneConnWriteTimeout`, so idle-session ping/pong messages repeatedly coincided with the write-timeout instant, making the race highly likely.

The `internal/wsconn` regression test (`TestLiveWebSocketSurvivesAWriteAtTheDeadlineInstant`) reproduces this with real
WebSockets: without the wrapper, 16/16 connections fail, all in <5 ms (without writing to the socket at all); with the wrapper, none fail.

However, `wsconn` can handle only the branch where no write is in progress when the callback arrives. If the same callback overlaps an active write, it directly
cancels the adapter's write context, which is never replaced. When coder/websocket sees the canceled write context, it closes the entire connection,
and all subsequent writes fail. This branch cannot be repaired by a wrapper. In production (mirror1), each node still disconnected 40–100 times per day,
with disconnection times strictly aligned to a 15 s cycle. Packet captures showed the closing side sending FIN less than 1 ms after sending its own keepalive ping/pong.
This was the same path; it also reproduced on local loopback (7 times in 17 minutes).

The root fix reduces `laneKeepAliveInterval` on both ends from 15 s
to 10 s, ensuring keepalive writes always renew at least 5 s before the midpoint of the write-timeout period and never hit the expiration instant.
`TestLaneYamuxConfig_MatchesPlan` (server) and `TestLaneYamuxConfig_KeepsKeepAliveClearOfTheWriteDeadline`
(agent) enforce this margin. After the change, local loopback ran for 25 minutes with zero disconnects, and 100 concurrent web terminals streaming output at full speed also had zero disconnects.

The remaining instability occurs when the host is genuinely saturated, causing sessions to drop and reconnect immediately, so the count remains report-only. **A node that disconnects and does not return** is a separate
condition and is asserted. For diagnostics, use `GPUWORKS_TESTENV_LOG=<path>` to enable server/agent logs and
`GPUWORKS_FLEET_DUMP_DIR=<dir>` to capture goroutine/mutex/block dumps as soon as a heartbeat becomes stale or a node disconnects
(adjust the threshold with `GPUWORKS_FLEET_DUMP_TRIGGER_MS`).

### Single-Process Teardown

The single-process configuration must stop its agents and close client connections when the test ends; otherwise, teardown fails for reasons unrelated to the behavior under test:

- `env.StopNodes(t)` cancels all agents and waits for the hub's session table to empty. `AddNode` cleanup only cancels
  the context without waiting, so server shutdown races with hundreds of sessions being torn down and exceeds testenv's 5 s shutdown budget.
- `client.CloseIdleConnections()` closes keep-alive connections left by churn. When the test itself holds hundreds of idle
  connections open, `http.Server.Shutdown` has to wait for them. Measurements show this is why the 1000-node disk-backed test encounters
  `server shutdown timeout` roughly half the time.

The multi-process configuration does not have this problem: the server is a separate process, and teardown simply sends it a signal.

### List Responses During Onboarding

The multi-process fleet test first onboards half the nodes and observes the steady state, then onboards the rest: 500 → 1000 by default.
During each onboarding phase and steady-state phase, it concurrently reads the full node list, paginated node list, deployment list, and instance list, reporting the sample count,
p95, maximum latency, and failure count. Each endpoint must have at least 1 successful sample during onboarding; the steady-state phase waits within a bounded period for at least 3 successful samples. For short onboarding phases, only requests already issued
are allowed to finish; new requests after onboarding do not count as onboarding samples. Requests must return HTTP 200, read the complete response body,
and take no more than 10 seconds each. Timeouts, error responses, read failures, and insufficient samples cannot be marked as passing.
Deployment/event list requests during churn also check for errors, sufficient samples, and the 10-second limit.

The deployment list already uses batched relation queries; the historical N+1 issue must no longer be described as the current performance root cause. Node onboarding also triggers image recovery and
storage scope resolution: these reads must be limited to the current node and its associated transfer/tag targets, to avoid each new node scanning the entire image catalog or
storage coverage of all nodes. `GPUWORKS_FLEET_READS=0` disables only concurrent reads during churn to isolate churn costs;
capacity checks during multi-process onboarding always run.

### Scaling and Prerequisites

Scale to the machine's capacity. These three environment variables apply to both configurations:

```bash
GPUWORKS_FLEET_NODES=200 GPUWORKS_FLEET_CHURN_NODES=100 GPUWORKS_FLEET_ROUNDS=1 \
GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 60m ./internal/integrationtest/fleet/ -run '^TestFleet1000NodesRedeployChurn$'
```

File descriptor (fd) requirements are approximately `6 × node count`. The test attempts to raise the `RLIMIT_NOFILE` soft limit to the hard limit. If it cannot,
it fails with `GPUWORKS_TEST_STRICT=1`; otherwise, it explicitly skips (it never silently runs with an insufficient limit). Non-Linux hosts
skip immediately: their fd limits and RSS measurement methods differ, so their results cannot be compared with CI.

## Public Access (expose)

All tests in `internal/server/expose` are offline. A fake cloudflared is provided through `PATH` (both `--version` and registration
logs use real output formats). An injectable `HealthChecker` performs the network health check used to determine whether an address is ready.
Tests replace this check to verify readiness decisions without relying on a real network. Process-related tests run real child processes: the test binary acts as the `connector-host`
helper, covering Stop cleanup, no orphans after the parent receives SIGKILL (including startup races, over multiple rounds), exit-code
propagation, and “no hosting when the server cannot acquire the hosting lock.” Upstream wire formats (including cloudflared `/config`,
deb indexes, and rpm repomirror metadata) are all covered by fixtures built from captured real traffic.

**Do not add permanent tests that require network access** (confirmed by the user): all download, extraction, and installation regressions use synthetic packages and local HTTP
fixtures. They do not access real package repositories, invoke package managers, or add Make targets or CI jobs. There are no “special cases”
enabled by environment variables either: those would still be network-dependent tests that deteriorate over time. Perform manual online verification during development as needed, recording versions,
environments, and results, but do not turn it into permanent test cases:

- **Package repository drift** (whether fixtures still match upstream): use `curl -s https://pkg.cloudflare.com/cloudflared/dists/any/main/binary-amd64/Packages | grep -E '^(Version|Filename|SHA256):'`
  and `curl -sI https://pkg.cloudflare.com/cloudflared/rpm/x86_64/cloudflared-linux-x86_64.rpm`
  to inspect deb index fields and check whether the rpm path is still unversioned. Fixture tests are the authoritative regressions for the parser and SHA256 validation;
  this step answers only “do the fixtures need to be captured again?”
- **End-to-end acquisition**: run `gpuworks expose up` on a host without cloudflared in PATH, and confirm
  that download, extraction, `--version` self-validation, and subsequent registration all succeed.
- **quick / named / agent**: registration and new-address availability, public `/healthz`, named token-only startup and route
  discovery, the first SSE chunk and cancellation, real agent onboarding, and recovery after network interruption are all manual development-time checks, not permanent test cases.

**Real-machine acceptance testing for named was only half complete on 2026-09-22** (the user subsequently decided not to expose this provider for now; see below). Real-machine verification confirmed
automatic connector acquisition (download from `pkg.cloudflare.com` + launch from an immutable version path), startup with only a token, successful registration
(`Registered tunnel connection`, with edge/protocol recorded as observations), `awaiting_route` and guidance when no route is published remotely,
and correct rejection of a tunnel with only the default catch-all (`version:-1`, `http_status:503`) as an available address.

**Unverified**:
Initial public reachability (promotion to `ready`), the first SSE chunk/cancellation, agent onboarding, and domain persistence after restart all require the user to first
publish a route in their own Cloudflare account. Because named is currently unavailable to users (`--provider cloudflare-named` is
explicitly rejected), these items cannot be accepted before it is enabled and must not be reported as passed.

## Architecture Gates

`make check-architecture` (included in `make test-unit`, in the same CI job as `check_time_policy.sh`)
runs two types of checks:

- **Architecture constraints**: each line in `scripts/arch-constraints.txt` is `key<TAB>limit<TAB>direction`. `max` means the measured value
  must be ≤ the limit; `min` means it must be ≥ the limit. The script prints `key measured/limit (direction)` for each entry and fails when a limit is violated.
- **Dependency direction**: leaf packages do not depend on store; `api` and `controller` do not depend on each other; `pkg` does not depend on `internal`;
  agent does not depend on server; and `proto` has no dependencies within this repository. These are structural rules, not budgets, and violations fail the check.

Ratchet rule: limits for `max` keys **only decrease**, and limits for `min` keys **only increase**. When a measured value is better than its limit, the script prints
a WARNING (without failing), prompting you to tighten the limit to the new measured value in the same MR. Any change that raises a `max` limit or lowers a `min`
limit must be explained in the MR description and receive explicit approval.

See [`scripts/arch-constraints.md`](../../scripts/arch-constraints.md) for each budget's intent, measurement command, and exclusions.
Fix matching patterns for false positives; do not adjust budgets.

Process/milestone labels (phase identifiers such as `M1`, `S0`, `G2`, and `P3`) **are prohibited in production code**, including identifiers,
comments, and filenames. The `process_name_refs` budget enforces this rule. Once a plan becomes obsolete, these labels become misleading
comments that readers cannot verify; replace them with descriptions of behavior. Test files are exempt and may freely reference specific phases.

Real-machine tests consistently use `l4-<provider-or-resource-pool>-<behavior>-<topology-or-external-dependency>`, with a one-to-one mapping between Go cases, Make targets,
and CI jobs. Verifiable behaviors use `node-onboarding`, `public-download`,
`deploy-chat`, `provisioning-matrix`, `one-vm-two-logical-nodes`, or
`two-vm-docker-distributed`; `nightly` and `manual` describe only trigger policies.

The repository currently provides explicit `make test-l4-*` targets for RunPod, AutoDL, Vast, CompShare, and Ascend resource pools.
Confirm the targets with `make help`/Makefile first, then run only the required test lanes.

## Cost and Caching

- Regular provider tests verify only machine rental, SSH, agent connectivity, and GPU inventory. API fields, regional constraints, and cleanup behavior for RunPod, CompShare, AutoDL,
  Vast, and ZStack are determined by each provider's implementation. Write tests against those implementations;
  do not assume uniform behavior.
- Public downloads are verified only by explicit, low-frequency `public-download` cases such as
  `test-l4-runpod-huggingface-public-download` and `test-l4-compshare-modelscope-public-download`,
  to avoid downloading images and models again for every rental.
- AutoDL Pro and Gitee Ascend have limited stock and are often sold out at the nightly peak (15:00 Asia/Shanghai). Both
  wait and retry only when every candidate reports no stock: for AutoDL every candidate spec returns "暂无库存", for Gitee
  every profile's create returns "没有找到符合条件的可用主机" (it has no stock API; only a create reveals supply).
  Retries run every 5 minutes with a default total wait of 30 minutes, overridden by `AUTODL_STOCK_WAIT` /
  `GITEE_STOCK_WAIT` (a Go duration such as `45m`; `0` disables). Nothing is created while waiting, and other errors
  (insufficient balance, authentication, price cap exceeded, and so on) are not retried; when AutoDL is asked for a
  specific card it neither swaps nor waits. The wait is added on top of the case's setup budget, so the `go test -timeout`
  and CI `timeout` of `test-l4-autodl-node-onboarding` and `test-l4-gitee-ascend-partition-capability` each grew by
  30 minutes. The AutoDL hourly price cap defaults to 10 CNY (`AUTODL_MAX_HOURLY_PRICE`).
- CompShare can reuse a prewarmed VM template through `COMPSHARE_GOLDEN_IMAGE_ID`, specify an image archive visible inside the VM through
  `COMPSHARE_DOCKER_IMAGE_TAR`, and register a complete model directory already present on each node through
  `COMPSHARE_PRELOADED_MODEL_PATH`.
- CompShare V100 / V100S (Volta, sm_70) cannot use the generic vLLM image: upstream's prebuilt kernels do not include sm_70,
  and the engine exits with `no kernel image is available for execution on the device`. When `l4-compshare-single-vm-tp2-dgpu`
  gets a V100S it uses `cuda12.8-v100-vllm1cat1.5.1`, built from 1Cat-vLLM (`build/runner-images`, the matrix item is marked
  `catalog: false` and stays out of the system image catalog), and asserts that the instances really started from that image.
  `COMPSHARE_V100_TEST_IMAGE` swaps the image, and `COMPSHARE_V100_DOCKER_IMAGE_TAR` names an image archive inside the VM (without one the platform
  pulls the image during the deployment startup window). Pascal (P40, sm_61) has no image that can run on it, so this case no longer rents it.
  The image must be built and published first (`build.sh`, then `publish.py`).
  `COMPSHARE_GPU_MODELS` (comma-separated, e.g. `V100S`) replaces the candidate card order the case rents from, to aim a run at one card type.
- `prepare-compshare-golden-image` creates and validates a new template, but does not delete old templates or change CI variables.
  The template contains both open-source and proprietary kernel modules for the same NVIDIA driver version (currently 570.153.02). At boot,
  `gpuworks-nvidia-kmod.service` loads the open-source modules first (Blackwell GPUs such as the RTX 5090 require them). If no GPU is initialized,
  it switches to the proprietary modules (GPUs without GSP, such as V100S and P40, require these). The selected variant is recorded in `/run/gpuworks-nvidia-kmod`.

  After creating the template, the tool boots and validates each GPU model specified by `-validate-gpus`. You can also use `-validate-image <id>` to validate only an existing template.
  GPU models that are out of stock or rejected by CompShare are reported without causing a failure. If the image lacks nvidia-container-toolkit, the tool installs the same version as the North China template and configures the Docker runtime
  (the Shanghai template line originally lacked it, preventing containers from accessing GPUs).

  Custom templates are isolated by region, and this account's API does not support cross-region copying.
  Create each region's template from an existing template in that region (`-region`/`-zone`/`-source-image`; when only 2080Ti×4 is available in Shanghai,
  use `-gpu-model 2080Ti -gpu-count 4`). Write the results to `COMPSHARE_GOLDEN_IMAGE_ID` and
  `COMPSHARE_ZONE_IMAGE_CN_SH2`.

  CompShare does not allow custom templates for A800 or H20
  (`RetCode 226603`), regardless of the driver. Template names longer than 42 characters are rejected during creation (`RetCode 230`).

When a test fails, first preserve the failure state and inspect process/container logs: retain the failed instance, record the run-id and SSH/agent logs,
and perform scoped cleanup limited to this run only after completing the investigation. Where supported by the implementation, cloud tests can set
`GPUWORKS_CLOUD_KEEP_ON_FAILURE=1`. Afterward, invoke
`test-l4-cleanup-leaked` or the corresponding cleanup entry point using only this run's scoped run-id. Account-wide cleanup without a scope is prohibited. After cloud L4 tests, use the run-id
to confirm that no instances, forwarding resources, or temporary files remain. For the product's rental workflow, see [Rent Cloud GPUs](../how-to/rent-cloud-gpus.md).

## Evidence

This guide documents testing methods. Put one-off run logs, screenshots, image digests, and environment facts in
`artifacts/work/<YYYY-MM-DD>-<topic>/<topic>-<YYYY-MM-DD>.md` (move them with the topic into
`artifacts/archive/` when the work is closed out). Current documentation only links to this evidence.
