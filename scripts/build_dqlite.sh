#!/usr/bin/env bash
# Build pinned dqlite dependencies locally; no package manager or global install.
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
assets="$script_dir/dqlite"
output=""
cache="${XDG_CACHE_HOME:-$HOME/.cache}/gpuworks/dqlite"
arch=amd64
compiler=musl-gcc
jobs=4
offline=false
force=false
go_sdk_only=false

usage() {
    cat <<'USAGE'
Usage: scripts/build_dqlite.sh --output DIR [--cache DIR] [--arch amd64]
       [--cc MUSL_CC] [--jobs N] [--offline] [--force] [--go-sdk-only]

Linux x86_64 only. Requires musl-gcc, gcc, make, cmake, binutils, Perl,
curl, tar/xz, unzip, pkg-config, flock, Python 3, Go and Linux UAPI headers.
The exact go-dqlite v3.0.4 source must already be in the Go module cache.
Set GO to an installed toolchain compatible with go.mod; SDK preparation never
downloads modules or toolchains.
Set GO to a compatible installed toolchain; SDK/toolchain downloads are disabled.
Pinned m4/autoconf/automake/libtool are built in CACHE, never installed globally.
The default cache is $XDG_CACHE_HOME/gpuworks/dqlite or ~/.cache/gpuworks/dqlite.
OUTPUT is a managed directory containing include/, lib/, env.sh and evidence/.
Source OUTPUT/env.sh before CGO builds; use Go -ldflags '-linkmode external'.
--offline requires every pinned source in cache/downloads; no network is used.
--force rebuilds this fingerprint only. ARM64 runtime support is not claimed.
--go-sdk-only updates the pinned private SDK/modfile/env in an existing verified OUTPUT;
it does not probe, build, or replace the native C libraries.
USAGE
}

die() { printf 'dqlite build: %s\n' "$*" >&2; exit 1; }
while (($#)); do
    case "$1" in
        --output|--cache|--arch|--cc|--jobs)
            (($# >= 2)) || die "missing value for $1"
            case "$1" in
                --output) output=$2;; --cache) cache=$2;; --arch) arch=$2;;
                --cc) compiler=$2;; --jobs) jobs=$2;;
            esac
            shift 2;;
        --offline) offline=true; shift;;
        --force) force=true; shift;;
        --go-sdk-only) go_sdk_only=true; shift;;
        -h|--help) usage; exit 0;;
        *) die "unknown argument: $1 (see --help)";;
    esac
done
[[ -n $output ]] || die '--output is required (use a dedicated build directory)'
[[ $arch == amd64 ]] || die "unsupported architecture: $arch; use amd64; ARM64 validation is deferred"
[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || die 'requires a Linux x86_64 build host'
[[ $jobs =~ ^[1-9][0-9]*$ ]] || die '--jobs must be a positive integer'

if [[ $go_sdk_only == true ]]; then
    [[ $force == false ]] || die '--go-sdk-only cannot rebuild C dependencies with --force'
    output=$(realpath -ms "$output")
    [[ $output != / && ! -L $output && -f $output/.gpuworks-dqlite && ! -L $output/env.sh && -f $output/env.sh ]] ||
        die '--go-sdk-only requires a managed native output and its env.sh'
    [[ -f $output/artifacts.sha256 ]] || die 'native artifact manifest is missing'
    (cd "$output" && sha256sum --status -c artifacts.sha256) || die 'native artifact verification failed'
    python3 "$assets/prepare_go_sdk.py" prepare --prefix "$output" --repo "$script_dir/.."
    mkdir -p "$output/evidence"
    cp "$output/go-sdk/manifest.json" "$output/evidence/go-sdk-manifest.json"
    sdk_source=$(printf 'source %q' "$output/go-sdk/env.sh")
    if ! grep -Fxq "$sdk_source" "$output/env.sh"; then printf '\n%s\n' "$sdk_source" >>"$output/env.sh"; fi
    printf 'dqlite build: private Go SDK ready; native libraries retained: %s\n' "$output" >&2
    exit 0
fi

for tool in gcc make cmake ar ld perl curl tar xz unzip patch pkg-config flock sha256sum \
    readelf file realpath find sort xargs sed awk cp mv mktemp timeout python3 go; do
    command -v "$tool" >/dev/null || die "missing $tool; install the build prerequisite and rerun (no packages are installed automatically)"
done
compiler=$(command -v "$compiler") || die 'musl compiler not found; install musl-tools or pass --cc /path/to/musl-gcc'
compiler=$(realpath -s "$compiler")
cache=$(realpath -m "$cache")
output=$(realpath -ms "$output")
for path in "$cache" "$output" "$compiler" "$script_dir"; do
    [[ $path != *[[:space:]]* ]] || die "build paths must not contain whitespace: $path"
done
[[ $cache != / && $output != / ]] || die 'cache and output must be dedicated directories, not /'
[[ $output != "$cache" && $cache != "$output/"* && $output != "$cache/"* ]] || die 'output and cache must be separate directories, neither containing the other'
[[ ! -L $output ]] || die "output must not be a symlink: $output"
[[ ! -e $output || -d $output ]] || die "output is not a directory: $output"
if [[ -d $output && ! -f $output/.gpuworks-dqlite ]]; then
    [[ -z $(find "$output" -mindepth 1 -maxdepth 1 -print -quit) ]] || die "output is not a managed or empty directory: $output"
fi
[[ -d /usr/include/linux && -d /usr/include/asm-generic && -d /usr/include/x86_64-linux-gnu/asm ]] ||
    die 'Linux UAPI headers missing; install linux-libc-dev for x86_64 (expected /usr/include/linux and x86_64-linux-gnu/asm)'
mkdir -p "$cache/downloads" "$cache/builds" "$(dirname "$output")"
exec 9>"$cache/.lock"
flock 9
probe_dir=$(mktemp -d "$cache/.probe.XXXXXX")
publish_dir=""
trap 'rm -rf -- "$probe_dir"; if [[ -n $publish_dir ]]; then rm -rf -- "$publish_dir"; fi' EXIT

# Ignore ambient flags: every target flag is part of this script's fingerprint.
unset CFLAGS CPPFLAGS CXXFLAGS LDFLAGS LIBS CONFIG_SITE REALGCC
export LC_ALL=C TZ=UTC SOURCE_DATE_EPOCH=1782977389
export PKG_CONFIG_PATH='' GIT_CEILING_DIRECTORIES="$cache"
run_logged() {
    local log=$1
    shift
    if "$@" >"$log" 2>&1; then return; fi
    tail -n 35 "$log" >&2
    die "command failed; full log: $log"
}

# The probe traces the actual musl linker inputs, including compiler wrapper specs.
# Hash those files and preprocessor include trees, not just the GCC version string.
cat >"$probe_dir/check.c" <<'C'
#include <features.h>
#ifdef __GLIBC__
#error A musl compiler is required, not glibc GCC
#endif
int main(void) { return 0; }
C
run_logged "$probe_dir/compiler.log" "$compiler" -static -Wl,--trace \
    "$probe_dir/check.c" -o "$probe_dir/check"
"$compiler" -v -E -x c /dev/null >/dev/null 2>"$probe_dir/includes"
{
    printf 'arch=%s\nsource_date_epoch=%s\n' "$arch" "$SOURCE_DATE_EPOCH"
    sha256sum "$script_dir/build_dqlite.sh" "$assets/sources.lock" "$assets/smoke.c" \
        "$assets/quorum-proof.patch" "$assets/existing-read-open.patch" \
        "$assets/shutdown-cancellation.patch" "$assets/shutdown-cancellation-regression.patch" \
        "$assets/check_shutdown_cancellation.py" \
        "$assets/go-dqlite-unix-socket.patch" "$assets/go-dqlite-node-store.patch" \
        "$assets/go-dqlite-node-store-regression.patch" \
        "$assets/go-dqlite-rpc-cancel.patch" "$assets/go-dqlite-rpc-cancel-regression.patch" \
        "$assets/check_rpc_cancel.py" "$assets/prepare_go_sdk.py" \
        "$assets/wal-index-region-gap.patch" \
        "$assets/gateway-leader-close.patch" "$assets/gateway-leader-close-regression.patch" \
        "$assets/gateway-query-write-abort.patch" "$assets/gateway-query-write-abort-regression.patch" \
        "$assets/check_gateway_query_write_abort.py" \
        "$assets/gateway-query-failure-rows.patch" \
        "$assets/uv-writer-close-off-loop.patch" \
        "$assets/uv-writer-close-off-loop-regression.patch" \
        "$assets/check_uv_writer_close.py" \
        "$compiler"
    for tool in gcc make cmake ar ld perl pkg-config; do
        command -v "$tool"
        "$tool" --version | sed -n '1,2p'
        sha256sum "$(command -v "$tool")"
    done
    "$compiler" --version
    "$compiler" -dumpspecs
    sha256sum "$probe_dir/check" | awk '{print "compiler_probe_sha256=" $1}'
    while IFS= read -r path; do
        sha256sum "$path"
    done < <(sed -n 's/^Reading specs from //p' "$probe_dir/includes")
    for program in cc1 as; do
        path=$("$compiler" -print-prog-name="$program")
        if [[ $path != */* ]]; then path=$(command -v "$path"); fi
        sha256sum "$path"
    done
    while IFS= read -r path; do
        [[ ! -f $path ]] || sha256sum "$path"
    done <"$probe_dir/compiler.log"
    while IFS= read -r path; do
        [[ -d $path ]] || continue
        find -L "$path" -type f -print0 | sort -z | xargs -0 -r sha256sum
    done < <(sed -n '/search starts here:/,/End of search list/p' "$probe_dir/includes" | sed -n 's/^ //p')
    find -L /usr/include/linux /usr/include/asm-generic /usr/include/x86_64-linux-gnu/asm \
        -type f -print0 | sort -z | xargs -0 -r sha256sum
} >"$probe_dir/fingerprint"
key=$(sha256sum "$probe_dir/fingerprint" | awk '{print $1}')
work="$cache/builds/$key"
prefix="$work/prefix"
logs="$work/logs"
printf 'dqlite build: fingerprint=%s\n' "$key" >&2

while read -r expected archive url; do
    [[ -n $expected && $expected != \#* ]] || continue
    download="$cache/downloads/$archive"
    if [[ ! -f $download ]]; then
        [[ $offline == false ]] || die "offline source missing: $download; rerun without --offline"
        printf 'dqlite build: downloading %s\n' "$archive" >&2
        run_logged "$probe_dir/download.log" curl --fail --location --silent --show-error \
            --proto '=https' --retry 3 --connect-timeout 15 --max-time 180 "$url" -o "$download.part"
        actual=$(sha256sum "$download.part" | awk '{print $1}')
        [[ $actual == "$expected" ]] || die "SHA256 mismatch for downloaded $archive: expected $expected, got $actual; inspect $download.part"
        mv "$download.part" "$download"
    fi
    actual=$(sha256sum "$download" | awk '{print $1}')
    [[ $actual == "$expected" ]] || die "cached SHA256 mismatch for $archive; remove $download and rerun online"
done <"$assets/sources.lock"

if [[ $force == true ]]; then rm -rf -- "$work"; fi
if [[ -f $work/complete && -f $prefix/artifacts.sha256 ]] &&
    (cd "$prefix" && sha256sum --status -c artifacts.sha256); then
    printf 'dqlite build: verified cache hit\n' >&2
else
    rm -rf -- "$work"
    mkdir -p "$work/src" "$work/build" "$logs" "$prefix/include" "$prefix/lib/pkgconfig"
    cp "$probe_dir/fingerprint" "$work/fingerprint.txt"
    cp "$assets/sources.lock" "$work/sources.lock"
    while read -r expected archive url; do
        [[ -n $expected && $expected != \#* ]] || continue
        case "$archive" in
            *.zip) unzip -q "$cache/downloads/$archive" -d "$work/src";;
            *) tar -xf "$cache/downloads/$archive" -C "$work/src";;
        esac
    done <"$assets/sources.lock"
    tools_prefix="$work/tools"
    export PATH="$tools_prefix/bin:$PATH"
    export ACLOCAL_PATH="$tools_prefix/share/aclocal"
    for package in m4-1.4.19 autoconf-2.72 automake-1.17 libtool-2.4.7; do
        printf 'dqlite build: building local %s\n' "$package" >&2
        mkdir -p "$work/build/$package"
        (
            cd "$work/build/$package"
            run_logged "$logs/$package-configure.log" env CC=gcc "$work/src/$package/configure" --prefix="$tools_prefix"
            run_logged "$logs/$package-build.log" make -j"$jobs"
            run_logged "$logs/$package-install.log" make install
        )
    done
    target_flags="-O2 -g0 -ffile-prefix-map=$work=/usr/src/gpuworks-dqlite"
    printf 'dqlite build: building libuv and SQLite\n' >&2
    run_logged "$logs/libuv-configure.log" cmake -S "$work/src/libuv-v1.51.0" -B "$work/build/libuv" \
        -DCMAKE_C_COMPILER="$compiler" -DCMAKE_C_FLAGS="$target_flags" -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX="$prefix" -DCMAKE_INSTALL_LIBDIR=lib -DLIBUV_BUILD_SHARED=OFF -DLIBUV_BUILD_TESTS=OFF
    run_logged "$logs/libuv-build.log" cmake --build "$work/build/libuv" -j"$jobs"
    run_logged "$logs/libuv-install.log" cmake --install "$work/build/libuv"
    sqlite="$work/src/sqlite-amalgamation-3500400"
    run_logged "$logs/sqlite-build.log" "$compiler" -O2 -g0 "-ffile-prefix-map=$work=/usr/src/gpuworks-dqlite" \
        -DSQLITE_ENABLE_DBSTAT_VTAB -DSQLITE_ENABLE_PREUPDATE_HOOK -DSQLITE_ENABLE_SESSION -DSQLITE_THREADSAFE=1 \
        -c "$sqlite/sqlite3.c" -o "$work/build/sqlite3.o"
    ar rcsD "$prefix/lib/libsqlite3.a" "$work/build/sqlite3.o"
    cp "$sqlite/sqlite3.h" "$sqlite/sqlite3ext.h" "$prefix/include/"
    for library in sqlite3 libuv; do
        version=3.50.4; link=sqlite3
        if [[ $library == libuv ]]; then version=1.51.0; link=uv; fi
        cat >"$prefix/lib/pkgconfig/$library.pc" <<PC
prefix=\${pcfiledir}/../..
libdir=\${prefix}/lib
includedir=\${prefix}/include
Name: $library
Description: GPUWorks pinned static musl $library
Version: $version
Libs: -L\${libdir} -l$link -lpthread -ldl -lm -lrt
Cflags: -I\${includedir}
PC
    done
    mkdir -p "$work/kernel-headers/asm" "$work/build/dqlite"
    ln -s /usr/include/linux "$work/kernel-headers/linux"
    ln -s /usr/include/asm-generic "$work/kernel-headers/asm-generic"
    cp /usr/include/x86_64-linux-gnu/asm/*.h "$work/kernel-headers/asm/"
    dqlite="$work/src/dqlite-91e3e2f90874e4ec3b45cde965f266342846531b"
    # This capability is part of the pinned build fingerprint. Never modify an
    # existing cache tree in place or silently reuse an unpatched native library.
    run_logged "$logs/dqlite-quorum-patch.log" patch --batch --forward --fuzz=0 \
        -d "$dqlite" -p1 -i "$assets/quorum-proof.patch"
    run_logged "$logs/dqlite-existing-read-patch.log" patch --batch --forward --fuzz=0 \
        -d "$dqlite" -p1 -i "$assets/existing-read-open.patch"
    run_logged "$logs/dqlite-shutdown-cancellation-patch.log" patch --batch --forward --fuzz=0 \
        -d "$dqlite" -p1 -i "$assets/shutdown-cancellation.patch"
    run_logged "$logs/dqlite-wal-index-region-gap-patch.log" patch --batch --forward --fuzz=0 \
        -d "$dqlite" -p1 -i "$assets/wal-index-region-gap.patch"
    run_logged "$logs/dqlite-gateway-leader-close-patch.log" patch --batch --forward --fuzz=0 \
        -d "$dqlite" -p1 -i "$assets/gateway-leader-close.patch"
    run_logged "$logs/dqlite-gateway-query-write-abort-patch.log" patch --batch --forward --fuzz=0 \
        -d "$dqlite" -p1 -i "$assets/gateway-query-write-abort.patch"
    run_logged "$logs/dqlite-gateway-query-failure-rows-patch.log" patch --batch --forward --fuzz=0 \
        -d "$dqlite" -p1 -i "$assets/gateway-query-failure-rows.patch"
    run_logged "$logs/dqlite-uv-writer-close-off-loop-patch.log" patch --batch --forward --fuzz=0 \
        -d "$dqlite" -p1 -i "$assets/uv-writer-close-off-loop.patch"
    printf 'dqlite build: generating and building dqlite\n' >&2
    (cd "$dqlite" && run_logged "$logs/dqlite-autoreconf.log" autoreconf -fi)
    (
        cd "$work/build/dqlite"
        run_logged "$logs/dqlite-configure.log" env CC="$compiler" CFLAGS="$target_flags" \
            CPPFLAGS="-isystem $work/kernel-headers" LDFLAGS='-static -Wl,-z,stack-size=1048576' \
            PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig" "$dqlite/configure" --prefix="$prefix" \
            --disable-shared --enable-static --with-static-deps --disable-backtrace --without-lz4
        run_logged "$logs/dqlite-build.log" make -j"$jobs"
        run_logged "$logs/dqlite-install.log" make install
    )
    # Installed metadata must remain usable without the original cache directory.
    for metadata in "$prefix/lib/pkgconfig/"*.pc; do
        # shellcheck disable=SC2016
        sed -i -e 's|^prefix=.*|prefix=${pcfiledir}/../..|' \
            -e 's|^exec_prefix=.*|exec_prefix=${prefix}|' \
            -e 's|^libdir=.*|libdir=${prefix}/lib|' \
            -e 's|^includedir=.*|includedir=${prefix}/include|' "$metadata"
    done
    sed -i 's|^Libs.private:.*|Libs.private: -lsqlite3 -luv -lpthread -ldl -lm -lrt|' \
        "$prefix/lib/pkgconfig/dqlite.pc"
    rm -f "$prefix/lib/"*.la
    mkdir -p "$prefix/bin"
    run_logged "$logs/smoke-link.log" "$compiler" -O2 -g0 -I"$prefix/include" "$assets/smoke.c" \
        -L"$prefix/lib" -ldqlite -lsqlite3 -luv -lpthread -ldl -lm -lrt \
        -static -Wl,-z,stack-size=1048576 -o "$prefix/bin/dqlite-smoke"
    (
        cd "$prefix"
        find include lib bin -type f -print0 | sort -z | xargs -0 sha256sum >artifacts.sha256
    )
    touch "$work/complete"
fi

# Cache reuse still validates the executable and exercises the current kernel.
mkdir -p "$logs"
{
    file "$prefix/bin/dqlite-smoke"
    readelf -hW "$prefix/bin/dqlite-smoke"
    readelf -lW "$prefix/bin/dqlite-smoke"
    readelf -dW "$prefix/bin/dqlite-smoke"
} >"$logs/elf.txt"
if grep -Eq 'INTERP|\(NEEDED\)' "$logs/elf.txt"; then die "dynamic ELF dependency detected; see $logs/elf.txt"; fi
grep -q 'Advanced Micro Devices X86-64' "$logs/elf.txt" || die 'unexpected ELF machine (expected x86-64)'
smoke_data=$(mktemp -d "$probe_dir/smoke.XXXXXX")
run_logged "$logs/smoke.log" timeout 30 "$prefix/bin/dqlite-smoke" "$smoke_data"
cat "$logs/smoke.log"

publish_dir=$(mktemp -d "$(dirname "$output")/.dqlite-output.XXXXXX")
cp -a "$prefix/." "$publish_dir/"
mkdir -p "$publish_dir/evidence"
cp "$logs/elf.txt" "$logs/smoke.log" "$work/fingerprint.txt" "$work/sources.lock" "$publish_dir/evidence/"
cp "$assets/quorum-proof.patch" "$assets/existing-read-open.patch" "$publish_dir/evidence/"
cp "$assets/shutdown-cancellation.patch" "$assets/shutdown-cancellation-regression.patch" \
    "$assets/check_shutdown_cancellation.py" "$publish_dir/evidence/"
cp "$assets/go-dqlite-rpc-cancel.patch" "$assets/go-dqlite-rpc-cancel-regression.patch" \
    "$assets/check_rpc_cancel.py" "$publish_dir/evidence/"
cp "$assets/wal-index-region-gap.patch" "$assets/gateway-leader-close.patch" \
    "$assets/gateway-leader-close-regression.patch" "$assets/gateway-query-write-abort.patch" \
    "$assets/gateway-query-write-abort-regression.patch" \
    "$assets/gateway-query-failure-rows.patch" "$publish_dir/evidence/"
cp "$assets/uv-writer-close-off-loop.patch" "$assets/uv-writer-close-off-loop-regression.patch" \
    "$assets/check_uv_writer_close.py" "$publish_dir/evidence/"
printf '%s\n' "$key" >"$publish_dir/.gpuworks-dqlite"
python3 "$assets/prepare_go_sdk.py" prepare --prefix "$publish_dir" --target-prefix "$output" --repo "$script_dir/.."
cp "$publish_dir/go-sdk/manifest.json" "$publish_dir/evidence/go-sdk-manifest.json"
{
    printf '# Source this file from Bash before building Linux amd64 with CGO.\n'
    printf 'export DQLITE_PREFIX=%q\n' "$output"
    printf 'export CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC=%q\n' "$compiler"
    printf 'export PKG_CONFIG_PATH= PKG_CONFIG_LIBDIR=%q\n' "$output/lib/pkgconfig"
    printf 'export CGO_CFLAGS=%q\n' "-I$output/include"
    printf 'export CGO_LDFLAGS=%q\n' "-L$output/lib -static -ldqlite -lsqlite3 -luv -lpthread -ldl -lm -lrt -Wl,-z,stack-size=1048576"
    printf 'source %q\n' "$output/go-sdk/env.sh"
} >"$publish_dir/env.sh"
# Only outputs explicitly marked as ours (or checked empty above) are replaced.
if [[ -e $output ]]; then mv "$output" "$publish_dir.previous"; fi
if ! mv "$publish_dir" "$output"; then
    [[ ! -e $publish_dir.previous ]] || mv "$publish_dir.previous" "$output"
    die "could not publish output: $output"
fi
rm -rf -- "$publish_dir.previous"
publish_dir=""
printf 'dqlite build: ready: %s\nfull build logs: %s\n' "$output" "$logs" >&2
