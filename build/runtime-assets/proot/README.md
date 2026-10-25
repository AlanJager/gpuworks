# Patched PRoot provenance and A/B build

GPUWorks embeds `internal/assets/bin/proot-{x86_64,arm64}-static-nofd` for rootless
udocker nodes. This directory records the source provenance, patches, and a
repeatable build for the existing nofd behavior and an experimental
full-no-socket variant.

## Source baseline

Both variants are pinned to upstream `proot-me/proot` commit:

```text
a0dc181e939c29d855f8a0fac12a484fd1014824
```

This is not the old `jorge-lip/proot-udocker` `udocker-1` branch. The base blob
ID in the historical nofd/full-no-socket patches (`792c79e`) resolves to this
upstream commit, and the current production binary's 2023 PRoot copyright,
`version_48.6`, and CLI source-line strings also identify the modern upstream
line. Fresh binaries built from the old 2019 fork fail the real udocker
invocation with `libm.so.6: cannot stat shared object: Invalid argument` when
`-k 6.8.0` is present.

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

The names are historical. In particular, `nofd` is not a claim that every
file-descriptor syscall is bypassed.

## Many-thread stress harness and the rejected fair-wait patch

PRoot serves every traced syscall of every thread from one process, and Linux
lists a process's tracees newest first, so with thousands of threads a saturated
tracer keeps returning the newest busy threads from `waitpid(-1)` and the older
ones sit in tracing-stop (seen with SGLang TP8 on 8 GPUs under runveil).

`stress/run-stress.sh <proot>` reproduces that: 8 processes with 300 threads
each, 60 of them looping on `stat()` plus a spawner churning short-lived
threads, and a main thread per process that must keep making progress.
`stress/smoke.sh <proot>` checks exit codes, fork/wait, a compile and threaded
Python. Use both when changing the event loop.

A patch that drains all pending `waitpid` results per round and indexes tracees
by pid fixed the stress case (main threads went from 0 to about 27 iterations/s)
but stalled real SGLang weight loading on an 8xH200 box, so it is **not** built
or shipped. The patch, the A/B table and a hypothesis are in
`artifacts/work/2026-10-20-runveil-many-threads/plan.md`; the shipped mitigation
is the thread-pool caps the server adds for SGLang on runveil nodes
(`internal/server/deploy/runveil_env.go`).

## Build and mandatory gates

Run the build in a native Linux Ubuntu 24.04 container of the target architecture (amd64 or arm64). The safe nofd variant is
the default. The script emits an evidence manifest next to the artifact:

```bash
out="$(mktemp -d /tmp/gpuworks-proot-output.XXXXXX)"
docker run --rm --platform linux/amd64 \
  -v "$PWD:/src:ro" -v "$out:/out" ubuntu:24.04 \
  env PROOT_OUTPUT_DIR=/out \
  bash /src/build/runtime-assets/proot/build.sh
```

Every artifact must pass the real udocker `-k 6.8.0` Python dynamic-loader
shape. The script also starts a guest AF_UNIX socket under `/tmp` while using a
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
