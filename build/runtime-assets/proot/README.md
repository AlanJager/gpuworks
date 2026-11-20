# Patched PRoot provenance and A/B build

GPUWorks embeds `internal/assets/bin/proot-{x86_64,arm64}-static-nofd` for rootless
udocker nodes. This directory records their source, the patches, and a
repeatable build for the nofd behavior and an experimental full-no-socket
variant.

## Source of the embedded binaries

Both are upstream `proot-me/proot` `a0dc181e` plus the nofd, temp-noise,
arm64-faccessat2, tracee-index and fair-adaptive patches, built with
`build.sh`, which writes an evidence manifest next to each artifact (patch
hashes, compiler, every gate's result, the artifact's own hash). The hashes in
`internal/assets/bin/manifest.sha256` are what ships.

- `proot-x86_64-static-nofd`: built in an `ubuntu:24.04` container with GCC
  13.3.0. It used to be an older build (upstream `704a4ab7` with the nofd
  patch only, predating `build.sh`), replaced when the event-loop patches went
  in, so both architectures now have the same source and the same gates.
- `proot-arm64-static-nofd`: built natively on an openEuler 24.03 ARM64 host
  (kernel 5.10) with GCC 12.3.1, because the ARM64 machines on hand either run
  a kernel too old for the path-resolution gate or cannot run a container at
  all. The predecessor was a GCC 7.3.0 build. The artifact is byte for byte
  the binary that produced the 910B measurements above
  (`8418b9fa9395818c7b47a9fbda201eb2f8b311a47c76b10f4f0cd34c7af1aabe`), so the
  numbers and the shipped binary are the same thing.

## Variants

- `patches/proot-nobrk-nofd.patch` preserves the historical production-style
  nofd baseline by removing `accept`, `accept4`, and `brk` from PRoot's seccomp
  trace list.
- `patches/proot-nosocket.patch` removes the same entries plus `bind`,
  `connect`, `getpeername`, `getsockname`, and `socketcall`. This is the
  experimental full-no-socket variant.
- `patches/proot-temp-noise.patch` silences a harmless guest-cwd restoration
  error in a talloc destructor. It is applied identically to both variants, so
  it is not an A/B variable.

- `patches/proot-arm64-faccessat2.patch` adds syscall 439 to the ARM64
  translation table. The pinned upstream already handles `faccessat2`, but
  only its x86 tables assigned the number. Without the ARM64 entry, recent
  glibc `realpath()` checks access host paths and incorrectly return `ENOENT`
  for guest directories, preventing CANN platform and operator discovery.

- `patches/proot-tracee-index.patch` indexes the tracees by pid and counts the
  terminated ones, so `get_tracee()` and `free_terminated_tracees()` stop
  walking the whole list on every event. It is a pure CPU change and keeps the
  order in which events are served.

- `patches/proot-fair-adaptive.patch` makes the event loop serve the oldest
  pending tracees first when that is worth its cost. Both are applied to every
  variant, like the two patches above, so neither is an A/B variable.

The names are historical. In particular, `nofd` is not a claim that every
file-descriptor syscall is bypassed.

## Many-thread fairness: the harness and the shipped event loop

PRoot serves every traced syscall of every thread from one process, and Linux
lists a process's tracees newest first, so with thousands of threads a saturated
tracer keeps returning the newest busy threads from `waitpid(-1)` and the older
ones sit in tracing-stop (seen with SGLang TP8 on 8 GPUs under runveil).

`stress/run-stress.sh <proot>` reproduces that: 8 processes with 300 threads
each, 60 of them looping on `stat()` plus a spawner churning short-lived
threads, and a main thread per process that must keep making progress.
`stress/smoke.sh <proot>` checks exit codes, fork/wait, a compile and threaded
Python. Use both when changing the event loop; `stress/gate_event_loop.sh`
runs a smaller, asserting version of both and is a mandatory build gate.

Two patches address it, and both are shipped in every variant.

`proot-tracee-index.patch` removes the two linear list walks per event
(`get_tracee()`, `free_terminated_tracees()`). It is the cheap half: tracer CPU
during a 4-rank vLLM start-up on 910B fell from 29% to 5.1% of a core, and from
22% to 8% for a 32B SGLang start-up on 8 H200. It does not change which tracee
is served next, so it does not fix starvation on its own.

`proot-fair-adaptive.patch` is the fairness half. Collecting every pending
`waitpid` result per round and serving them oldest-first does equalize the
tracees, but the `WNOHANG` call that ends a collection has to walk the whole
children list to find nothing, and that walk costs about 43 ns per tracee
(measured: 18 us at 500 tracees, 94 us at 2200, 136 us at 3000). Paying it on
every round is what made an earlier unconditional version unusable: loading a
122B checkpoint on 8 H200 took 615 s instead of 35 s, with the tracer at 100%
system time. So the patch collects only while a collection pays for itself: a
round that yields at least `nb_tracees / 37` events keeps collecting, and below
that the interval between collections doubles up to 1024 rounds. Phases with
many threads in flight (warmup, kernel compilation) get the fairness; phases
with few (reading weights from storage) fall back to the upstream loop and pay
nothing. `PROOT_FAIR_BATCH=1` restores the upstream loop exactly, which is the
escape hatch if a workload ever regresses; `PROOT_FAIR_BATCH=n` caps a
collection at n events and `PROOT_FAIR_ADAPTIVE=0` collects on every round,
both only useful for measuring.

Measured three ways, all single runs:

| Case | Upstream loop | Shipped (adaptive) |
| --- | --- | --- |
| harness, 2200 threads, 16 busy | 182.5k syscall-stops/s, 5731 scans/s | 181.6k stops/s (-0.5%), 174 scans/s, interval backs off to 1024 |
| harness, 1700 threads, 512 busy | fair only with an unconditional collection | 64.3k stops/s, served min/max 1.01, interval stays at 1 |
| SGLang 122B, TP8, 8 H200, same machine | weight load 29.4-61.1 s, rank init 24.9-100.5 s | weight load 29.7-38.0 s, rank init 7.1-23.7 s |
| vLLM Qwen3.8-Flash-Next W4A8 MTP, TP4, 910B | warmup 72 / 91 / 2766 / 2899 s, worker CPU spread 4.9x, 96 min to ready | warmup 58.4 / 61.3 / 64.0 / 66.4 s, worker CPU spread 1.21x, 15m44s to ready |

The 910B run is the honest headline: the same start-up that spent 48 minutes
with two of four ranks starving in warmup now finishes warmup within 8 seconds
of each other, with no environment variable set. The 8 H200 comparison is
against the upstream loop on the same machine (`PROOT_FAIR_BATCH=1`); the
earlier 615 s unconditional-collection figure came from a different rented
machine and was not re-measured there, so it bounds the risk of collecting
unconditionally, not the behaviour of this patch.

Two other designs were measured and rejected: FIFO aging (timestamp every stop
and serve in time order) deadlocks at low concurrency, because the queue never
fills and throughput collapses to 20 events/s, and a timer only converts the
deadlock into a latency bound; a rotating cursor over the tracee list costs
only 2.7% once amortized but does not equalize (served-count skew 82952 against
the upstream 128971 in the harness).

One tracer still has a saturation point, and the harness finds it. At 8 processes
with 300 threads each (2200-2400 tracees, 480 of them busy) the shipped loop no
longer equalizes: 6 of the 8 main threads fell below 100 iterations and the
served gap reached 19.7 s, although setup still completed in 20.6 s. The
upstream loop collapsed outright on the same run—all 8 mains at 0 iterations,
setup never finishing inside the 120 s cap—and `PROOT_FAIR_ADAPTIVE=0` was no
better (8 of 8 starved), so the back-off is not what limits it: the event rate
from 480 continuously busy tracees exceeds what one tracer can serve. The same
binary is perfectly fair at 4 processes with 150 threads each (600 tracees, 40
busy: 2320 iterations minimum, 9 ms maximum gap). The harness load is synthetic,
since its busy threads never stop calling `stat()`, where a real start-up is
bursty: a 4-rank vLLM start-up on 910B creates about 510 threads and is fixed
completely, matching the 4-process row. Measured on an 8 x 910B2C x86_64 host,
128 cores, kernel 5.15, with the shipped binary.

The thread-pool caps the server adds for SGLang on runveil nodes
(`internal/server/deploy/runveil_env.go`) stay as they are: they reduce how
many threads are created, which is useful whatever the tracer does.

## Build and mandatory gates

Run the build in a native Linux Ubuntu 24.04 container of the target
architecture (amd64 or arm64), on a host kernel of **5.8 or newer**: the
path-resolution gate is deliberately strict and asserts `faccessat2`, which
older kernels do not have, so an older host fails the build with
`guest faccessat2: Function not implemented` (a 4.19 ARM64 host does). The safe
nofd variant is the default. The script emits an evidence manifest next to the
artifact:

```bash
out="$(mktemp -d /tmp/gpuworks-proot-output.XXXXXX)"
docker run --rm --platform linux/amd64 \
  -v "$PWD:/src:ro" -v "$out:/out" ubuntu:24.04 \
  env PROOT_OUTPUT_DIR=/out \
  bash /src/build/runtime-assets/proot/build.sh
```

Two inputs can be supplied locally when the network will not cooperate:
`PROOT_TALLOC_TARBALL=<file>` replaces the download from download.samba.org
(the pinned sha256 is still what is checked, and Ubuntu's
`talloc_2.4.2.orig.tar.gz` is the same tarball), and `PROOT_REPOSITORY=<path>`
accepts a local clone or `git bundle` of PRoot instead of cloning from github
(the commit is pinned by sha, and the manifest records the source used).

Every artifact must pass the real udocker `-k 6.8.0` Python dynamic-loader
shape, the guest path-resolution gate and the event-loop gate
(`stress/gate_event_loop.sh`: exit codes, a signal death, 300 forks and their
wait, a compile-and-run, 64 Python threads, shell job control, and a
many-thread fairness run in which no rank may fall under 100 iterations). The script also starts a guest AF_UNIX socket under `/tmp` while using a
separate guest root and verifies that a successful bind never creates the exact
guest pathname directly in the host `/tmp` namespace. For long translated
paths PRoot may use a private short temporary binding, so the literal
`guest-root/tmp/name.sock` need not exist while the socket is alive.

The AF_UNIX result is only authoritative on native Linux amd64. Docker Desktop
may execute linux/amd64 through Rosetta, whose ptrace/seccomp behavior is not
equivalent; the script detects that path, records `not-run-emulated`, and
refuses to certify the artifact.

The full-no-socket variant passes the Python gate but fails the AF_UNIX gate:
removing `bind`/`connect` from PRoot's trace list also disables pathname
translation for Unix-domain sockets. A guest `/tmp/name.sock` is then created
directly as host `/tmp/name.sock`, violating container isolation and permitting
cross-container name collisions.

For forensic reproduction only, build it with:

```bash
PROOT_VARIANT=nosocket PROOT_ALLOW_UNSAFE_NOSOCKET=1 \
  PROOT_OUTPUT_DIR=/out bash build/runtime-assets/proot/build.sh
```

The manifest will still contain `af_unix_path_isolation=fail`. This override is
not permission to embed or deploy the artifact.

## Current evidence and rollout rule

On the same CompShare rootfs and identical Ubuntu 24.04/GCC 13/talloc 2.4.2
toolchain, both `a0dc181e` variants passed the `-k 6.8.0` Python gate. A strict
interleaved loopback microbenchmark showed about 24.5% higher short-lived
connect/accept operations for full-no-socket, but about 8% lower single-stream
throughput. The latter transfer does not exercise the removed syscalls and is
treated as noise or a non-benefit, not as evidence of a general speedup.

Because full-no-socket fails pathname isolation, it must not replace
`internal/assets/bin/proot-x86_64-static-nofd` and should not proceed to a
general vLLM/NCCL rollout. A future optimization would need to retain AF_UNIX
path translation—for example by distinguishing address families safely—then
pass image pull, unpack, two real vLLM ranks, NCCL, `/v1/models`, gateway chat,
and exact run-id cleanup.

### Guest path resolution gate

`gate_path_resolution.c` exercises the actual built binary with a directory
that exists only through a guest bind. It checks `faccessat2(AT_EACCESS)`,
dirfd-relative access, missing-path errors, and `realpath` with a symlink,
parent traversal, and a trailing slash. Run it on the target architecture;
an amd64 pass cannot certify the ARM64 syscall-number table. The build runs
this gate for each variant and records it in the evidence manifest. The
asset unit test also runs it against the embedded binary on native Linux.
It probes `faccessat2` directly on the host first: only host `ENOSYS` selects
`--realpath-only`, so older kernels still exercise guest path resolution.
If the host supports the syscall, all direct-access assertions remain
mandatory; an `ENOSYS` from the guest fails the gate. Other host errors fail
rather than reducing coverage. Per-process seccomp fixtures cover all three
outcomes without changing the host. The build-time gate remains strict.
