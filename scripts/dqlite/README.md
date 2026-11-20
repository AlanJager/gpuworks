# Local dqlite dependency build

`../build_dqlite.sh` builds Linux amd64 static musl libraries for
`gpuworks-dqlite`, the database helper of a clustered control plane, and a
patched private copy of the go-dqlite SDK. It installs no system packages.
ARM64 execution is intentionally outside this script's current support.

The server itself stays a pure Go binary (`CGO_ENABLED=0`). It starts
`gpuworks-dqlite` from its own directory and speaks the dqlite wire protocol to
it; the helper is the only program linked against these libraries.

```bash
make dqlite-deps                # tmp/dqlite/mainline-amd64, cache tmp/dqlite/cache
make dqlite-helper              # bin/gpuworks-dqlite-linux-amd64 (static, cgo)
make server-linux-amd64-dqlite  # bin/gpuworks-linux-amd64-dqlite (pure Go)
make test-dqlite-helper
```

The recipe never downloads a Go toolchain. The Make targets pass the toolchain
that `go.mod` selects (`DQLITE_GO`, default `$(go env GOROOT)/bin/go`); by hand,
export `GO` to an installed toolchain that satisfies `go.mod`. `DQLITE_FLAGS=--offline`
forbids source downloads when `DQLITE_CACHE/downloads` is already populated.

By hand, the helper is built with the full environment and the server with the
SDK environment only:

```bash
scripts/build_dqlite.sh --output "$PWD/tmp/dqlite/mainline-amd64" \
  --cache "$PWD/tmp/dqlite/cache" --jobs 6
(source tmp/dqlite/mainline-amd64/env.sh &&
  go build -tags=dqlite,nosqlite3 -ldflags '-linkmode external' ./cmd/gpuworks-dqlite)
(source tmp/dqlite/mainline-amd64/go-sdk/env.sh &&
  CGO_ENABLED=0 go build -tags=dqlite,nosqlite3 ./cmd/gpuworks)
```

`-tags=nosqlite3` makes go-dqlite keep node metadata in YAML instead of linking
go-sqlite3. `env.sh` sets `CGO_ENABLED=1`, `GOOS=linux`, `GOARCH=amd64`, `CC`,
isolated pkg-config paths, include paths and static linker flags including
`-Wl,-z,stack-size=1048576`, then sources `go-sdk/env.sh`, which only adds
`-modfile=<prefix>/go-sdk/gpuworks-native.mod` to `GOFLAGS`. Source either file
in a subshell if other targets follow. The modfile is a copy of the repository
`go.mod` with go-dqlite replaced by the patched copy; after `go.mod` or `go.sum`
changes, rerun the script (or `--go-sdk-only`) so the copy matches.

Both binaries use the patched SDK: the unix-socket patch matters to the helper,
while the node-store and RPC-cancellation patches change the pure Go client and
driver the server uses.

Build hosts require musl-gcc, native GCC, Make, CMake, binutils, Perl, curl,
tar/xz, unzip, patch, pkg-config, flock and Linux UAPI headers. The current native x86
header layout is `/usr/include/{linux,asm-generic,x86_64-linux-gnu/asm}`; the
script reports missing prerequisites instead of changing the host. On Debian
or Ubuntu these normally come from `musl-tools`, `build-essential`, `cmake`,
`perl`, `curl`, `xz-utils`, `unzip`, `pkg-config`, `util-linux` and
`linux-libc-dev`. Output and cache must be separate dedicated directories, and
build paths must not contain whitespace. An existing nonempty output needs the
script's ownership marker before it can be replaced.

## Pinned inputs and cache behavior

`sources.lock` pins SHA256 and HTTPS URLs for dqlite 1.18.7 at its exact commit,
SQLite 3.50.4, libuv 1.51.0, m4 1.4.19, autoconf 2.72, automake 1.17 and libtool
2.4.7. The autotools are built into the cache; upstream's pinned source macro
files supply the additional autoconf checks. No moving config.guess/config.sub,
global package installation or preexisting experiment directory is used.

`quorum-proof.patch` adds the repository's CLUSTER format 2 capability to that
exact dqlite source. It appends a fresh Raft barrier and returns its term, index
and committed configuration only while those facts remain stable. Format 0/1
and the existing SQL barrier keep their original behavior. Applying the patch
uses zero fuzz in a new fingerprint directory; existing cache sources remain
untouched. This changes native vendor behavior and adds a barrier entry to the
Raft log, while the proof request performs no SQL writes. An unpatched library
cannot supply this capability.

`existing-read-open.patch` is applied after the quorum patch. It adds request
20 (`OPEN_EXISTING_READ`), schema 0, with the existing OPEN body and DB response
(id and reserved word both zero). It requires flags 0, VFS `volatile`, no extra
body bytes, and a plain filename of 1–511 bytes: no ASCII control bytes, slash,
backslash, colon, question mark, `.` or `..`. Other filename bytes are compared
exactly against the existing registry. Restore and applied OPEN commands populate
that registry; this request refuses a missing record and opens SQLite with
`SQLITE_OPEN_READONLY | SQLITE_OPEN_EXRESCODE`, without CREATE. The volatile VFS
reports the actual access flags to SQLite, and its main-file handle refuses writes
and truncation when read-only. The strict opener verifies SQLite actually reports
the main database as read-only. Empty databases are refused before bootstrap
PRAGMAs. Ordinary OPEN keeps its original behavior.

`shutdown-cancellation.patch` then protects a committed log prefix when
`appendLeaderCb` receives `RAFT_CANCELED` after the Raft node has become
`RAFT_UNAVAILABLE`. Followers may have committed and applied an entry while the
leader's local append remains pending; closing cancels that append. Truncating
the applied prefix would violate its state machine. The patch truncates only
the uncommitted suffix and keeps reference release, transition assertions and
`last_stored` unchanged. It does not fix live I/O errors, cancellation callbacks
after a leader steps down, or in-flight writes returning I/O errors during close.

The offline regression below copies the normal recipe's patched source to a
new directory, reverses this fix for two exact assertion failures, then reapplies
it and checks the applied prefix, uncommitted suffix and heap-checked teardown.
It compares the original and candidate core suites, including existing skips.
The fixture uses three nodes and delayed leader persistence with real quorum
application, then synthetic queued cancellation callbacks after the real
unavailable conversion. It does not execute `uvAppendClose`, physical disk I/O
or restart, and provides no remote recovery measurement. The test-only fixture
patch is applied only to this isolated copy, never the published runtime library.

```bash
# KEY is the fingerprint in OUTPUT/.gpuworks-dqlite; use a new WORK directory.
python3 scripts/dqlite/check_shutdown_cancellation.py \
  --source "CACHE/builds/KEY/src/dqlite-91e3e2f90874e4ec3b45cde965f266342846531b" \
  --dependency-prefix OUTPUT --kernel-headers "CACHE/builds/KEY/kernel-headers" \
  --work-dir WORK --jobs 2
```

The regression writes private logs and `WORK/receipt.json`, refuses a work
directory that exists or is inside the input source, dependency prefix or
headers, disables core dumps for expected failures, and verifies the
input SDK archive remains unchanged. Its compiler warning exception is limited
to upstream test runner `suggest-attribute=noreturn`; runtime build flags remain
the normal recipe's flags.

An older native library rejects request 20; callers must propagate that failure
without falling back to ordinary OPEN. This capability protects the main database
open; it is not a general SQL sandbox. Its consumer must use fixed reads and
transaction control. Successful OPEN alone provides neither freshness nor a
quorum proof. The smoke does not qualify the new request's application behavior;
that requires the directed native protocol and restored-database tests.

Archives are verified before extraction on every invocation. A corrupt cache
entry is rejected with its path; remove that archive and rerun online after
checking the cause. Interrupted downloads never replace verified archives.
`--offline` prohibits downloads and requires all seven archives in
`CACHE/downloads`. `--force` rebuilds the current fingerprint without deleting
other cached versions.

The build fingerprint includes script/source/smoke/patch content, compiler identity,
compiler and linker inputs, selected build-tool versions and executables, musl
headers and Linux UAPI headers. A different fingerprint builds a separate
directory. Completed artifacts carry their own SHA256 inventory; corruption
invalidates a cache hit. A cache lock serializes builds sharing that cache.
Compiler and system headers are supplied by the build host, so CI must pin its
build image as well as the source lock for reproducibility across machines.

The target build explicitly disables shared libraries, LZ4 and backtraces.
SQLite enables `DBSTAT_VTAB`, `PREUPDATE_HOOK`, `SESSION` and `THREADSAFE=1`.
Ambient compilation flags are cleared; changing target flags requires a
reviewed script change and therefore a new fingerprint.

## Evidence and failures

Every successful invocation, including a cache hit, checks x86-64 ELF headers,
rejects `INTERP` and dynamic `NEEDED` entries, and runs the C smoke on the native
kernel with a 30-second timeout. The smoke checks dependency versions and
required SQLite compile options, then starts/stops a loopback dqlite node using
an ephemeral bind port. The temporary node data is removed after the run.
Native Linux AIO support is required; a smoke failure is a failure, never a
silent skip.

The published output has `include/`, `lib/`, `bin/dqlite-smoke`, `env.sh`,
`artifacts.sha256`, and `evidence/` containing source pins, input fingerprint,
ELF details, the native patch and smoke output. Complete build logs stay under the fingerprint's
cache directory; the script prints the failing log path and its last lines.
An existing published output is retained if a rebuild or smoke fails.

This evidence proves native dependency construction and node startup only. It
does not establish GORM correctness, SQL replication, control-plane HA, full
release package size or ARM64 runtime behavior.

## Discovery-cache persistence regression

The pinned private Go SDK also applies `go-dqlite-node-store.patch`. It keeps the
real one-second membership-query/role-adjustment loop. Only a repeated Set of
exactly the same ordered ID/address/role list can avoid replacing `cluster.yaml`,
and only after this store has successfully persisted it and the same private
regular file still contains the exact bytes. First Set after startup, changed
membership, missing/damaged/replaced files, and previous write failures retain
the original write/error path. Successful writes own a copy of caller input.
No grant, readiness timer, SQL commit, or transport-policy directory sync changes.
The upstream background loop's handling of Set errors is unchanged.

`prepare_go_sdk.py` pins both patch inputs and resulting files, verifies the full
upstream source, and includes the node-store patch in the private SDK manifest.
The exact previous modfile helper can migrate; an unknown helper is rejected even
if its manifest is rehashed. Use a new native output for a new candidate; frozen
runtime SDK directories and the shared Go module cache must remain untouched.

The separate regression patch is applied only to a private fixture module:

```bash
python3 scripts/dqlite/test_go_sdk.py
python3 scripts/dqlite/check_node_store.py \
  --source "$(go env GOMODCACHE)/github.com/canonical/go-dqlite/v3@v3.0.4" \
  --work-dir /absolute/new/private-fixture --go /absolute/local/go
```

The latter requires local GCC and strace, makes no network requests, and runs the
real Go file store under race/vet. It proves actual fsync counts for repeated,
startup, changed and repaired snapshots, and injects a real fsync EIO to verify
same-value retry after failure. It also checks rename/create errors, all member
fields/order, concurrent reads/writes, alias isolation and file repair. It does
not build a server, run native consensus, measure HA recovery or claim that
renameio's file-sync/rename sequence adds parent-directory crash durability.

## RPC cancellation regression

The private SDK applies `go-dqlite-rpc-cancel.patch` after the Unix-socket and
node-store patches. It observes cancellation while a protocol request or row
continuation is blocked on the wire, including contexts without a deadline and
early cancellation before a future deadline. The callback closes only the
captured underlying connection; the request owner stops and joins that callback
before returning. Cancellation of a direct standard TLS connection discards its
underlying wire without waiting for a close-notify exchange. Interrupted streams
cannot return to the shared protocol pool. Later closes reuse the first physical
close outcome, preserving actual cleanup errors without closing the socket again.

An interrupted request keeps its context error, including any connection-close
error, so `database/sql` cannot replay a potentially executed statement as
`ErrBadConn`. A later request that has sent nothing on the discarded stream may
replace the connection. Losing a COMMIT response still leaves its outcome
unknown; cancellation does not establish that it rolled back.

The SDK manifest pins both changed protocol/driver files, the production patch,
and the complete resulting module. Verification reconstructs the exact original
source inventory. Known previous SDK formats remain readable, but preparing a
candidate requires a new owned prefix; do not modify a frozen runtime prefix or
the shared module cache. `test_go_sdk.py` checks the packaging and tamper rules.
The separate RPC regression patch and checker run only in private fixture
modules and do not enter the runtime SDK. Their wire and driver tests must be
combined with the real native database cancellation tests before qualifying a
server. These tests alone do not measure remote HA recovery.

With a compatible native `env.sh` sourced for its compiler and C libraries:

```bash
python3 scripts/dqlite/check_rpc_cancel.py \
  --source /absolute/frozen-prefix/go-sdk/module \
  --work-dir /absolute/new/rpc-cancel-fixture --go /absolute/local/go
```

The source must be the exact previous Unix-socket plus node-store SDK. The
checker refuses an existing work directory, verifies the input tree before and
afterward, requires the old source to fail the cancellation assertions, then
applies the production patch with zero fuzz and runs the wire/driver tests under
normal execution, race detection and vet. Native C libraries and cached Go
dependencies must already be available; network downloads are disabled. Its
`receipt.json` records actual exits. Opaque connection wrappers that do not
expose `NetConn` still close through their own implementation; the repository's
owned snapshot socket is such a wrapper. Do not infer a direct TCP-close bound
for those connections from the direct-wire tests.

## WAL-index region gap and gateway leader close

`wal-index-region-gap.patch` (applied after `shutdown-cancellation.patch`)
fixes the `vfsMainFileShmMap` assertion `region_index ==
f->mappedShmRegions.len`, which aborted two field members. Check it with
`wal-index-region-gap-test.c` against the source tree matching the SDK
fingerprint:

```bash
# Source OUTPUT/env.sh first. SOURCE is CACHE/builds/KEY/src/dqlite-<commit>.
"$CC" -O2 -Wall -Wextra -Werror -I"$DQLITE_PREFIX/include" -I"$SOURCE/src" \
  scripts/dqlite/wal-index-region-gap-test.c -L"$DQLITE_PREFIX/lib" \
  -ldqlite -lsqlite3 -luv -lpthread -ldl -lm -lrt -static \
  -Wl,-z,stack-size=1048576 -o /tmp/wal-index-region-gap-test
for case in region-gap shared-existing-region no-extend-missing-region repeated-map-unmap; do
  /tmp/wal-index-region-gap-test "$case" || exit 1
done
```

`gateway-leader-close.patch` keeps leader cleanup when an exec allocation
fails after the request was recorded. `check_gateway_leader_close.py` reverses
the fix in a disposable copy, requires the injected failures, then requires
the fixed gateway suite to pass:

```bash
python3 scripts/dqlite/check_gateway_leader_close.py --source SOURCE \
  --dependency-prefix OUTPUT --kernel-headers CACHE/builds/KEY/kernel-headers \
  --work-dir NEW_WORK_DIR --cc /usr/bin/musl-gcc --jobs 2
```

These fix the allocation-failure path only. `gateway-query-write-abort.patch`
fixes the ordinary-shutdown `db__close: db->leaders == 0` abort: a query
yielding rows resumes only from the connection's write callback, so a client
that drops its socket mid-result (write error) left the deferred gateway close
waiting forever with its leader open. The failed write now resumes the
cancelled query so the gateway finalizes its leader. A client of any member
can trigger this, so Go-side connection ownership alone cannot prevent it.
`check_gateway_query_write_abort.py` reverses the fix in a disposable copy,
requires the mid-ROWS client-drop case to abort with the gateway still open,
then requires it and every other `conn/` case to pass with the fix:

```bash
python3 scripts/dqlite/check_gateway_query_write_abort.py --source SOURCE \
  --dependency-prefix OUTPUT --kernel-headers CACHE/builds/KEY/kernel-headers \
  --work-dir NEW_WORK_DIR --cc /usr/bin/musl-gcc --jobs 2
```

`uv-writer-close-off-loop.patch` keeps the raft loop thread running while a
segment writer is closed. Each finished log segment closes its writer, and the
writer destroyed its kernel AIO context and closed its file descriptor on the
loop thread. `io_destroy(2)` sleeps uninterruptibly until the context is
quiescent, with no upper bound while a write is stuck in the kernel, and the
loop thread is the one that sends heartbeats: a leader that is away for longer
than the election timeout is deposed. The patch releases both in the libuv
thread pool and fires the writer's close callback on the loop thread
afterwards; if the work cannot be queued it releases on the loop thread as
before. The raft I/O instance counts the segment writers that are still being
released and does not report closed while any remain.
`check_uv_writer_close.py` reverses the fix in a disposable copy, links the
raft tests with `--wrap=io_destroy` so the kernel call takes 500 ms, requires
the loop's 10 ms timer to observe that gap, then requires with the fix that
the gap stays below 250 ms, that an instance closed during a slow release
waits for it, that 300 segments filled back to back with a slow release reload
intact, and that every storage case of both raft uv test programs passes:

```bash
python3 scripts/dqlite/check_uv_writer_close.py --source SOURCE \
  --dependency-prefix OUTPUT --kernel-headers CACHE/builds/KEY/kernel-headers \
  --work-dir NEW_WORK_DIR --cc /usr/bin/musl-gcc --jobs 2
```

`gateway-query-failure-rows.patch` makes a query that fails after it has
produced rows report its own error. The failed step left the rows it had
already encoded in the response buffer and the failure was appended behind
them, so a client read the column count as the error code and the first column
name as the message: a write that lost a snapshot conflict (SQLite 517,
"database is locked") on `INSERT ... RETURNING token` arrived as error 2
"token", which no caller can classify or retry. The failure now replaces the
encoded rows. `TestQueryThatFailsAfterARowReportsItsOwnError` in
`cmd/gpuworks-dqlite` fails against a library without the patch and passes
with it (`make test-dqlite-helper`).
