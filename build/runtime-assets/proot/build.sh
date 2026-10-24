#!/bin/bash
# Build auditable static PRoot nofd/full-no-socket A/B candidates.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PATCH_DIR="$HERE/patches"
PROOT_REPOSITORY=https://github.com/proot-me/proot.git
PROOT_COMMIT=a0dc181e939c29d855f8a0fac12a484fd1014824
TALLOC_VERSION=2.4.2
TALLOC_URL="https://download.samba.org/pub/talloc/talloc-${TALLOC_VERSION}.tar.gz"
TALLOC_SHA256=85ecf9e465e20f98f9950a52e9a411e14320bc555fa257d87697b7e7a9b1d8a6
VARIANT="${PROOT_VARIANT:-nofd}"
OUTPUT_DIR="${PROOT_OUTPUT_DIR:-$HERE}"
BUILD_ROOT="$(mktemp -d /tmp/gpuworks-proot-build.XXXXXX)"
trap 'rm -rf "$BUILD_ROOT"' EXIT

case "$VARIANT" in
	both|nofd|nosocket) ;;
	*) echo "PROOT_VARIANT must be both, nofd, or nosocket" >&2; exit 2 ;;
esac
if [ -n "${PROOT_OUTPUT:-}" ] && [ "$VARIANT" = both ]; then
	echo "PROOT_OUTPUT requires a single PROOT_VARIANT; use PROOT_OUTPUT_DIR for both" >&2
	exit 2
fi
mkdir -p "$OUTPUT_DIR"

apt-get update -q
apt-get install -y -q \
	binutils ca-certificates curl file gcc git make patch pkg-config python3

git clone -q "$PROOT_REPOSITORY" "$BUILD_ROOT/proot-source"
git -C "$BUILD_ROOT/proot-source" checkout -q "$PROOT_COMMIT"

curl --fail --location --silent --show-error "$TALLOC_URL" -o "$BUILD_ROOT/talloc.tar.gz"
echo "$TALLOC_SHA256  $BUILD_ROOT/talloc.tar.gz" | sha256sum --check --status
tar xzf "$BUILD_ROOT/talloc.tar.gz" -C "$BUILD_ROOT"

# talloc's waf build normally emits a shared object. Generate config.h, then
# compile the two objects needed by PRoot into a private static archive.
cd "$BUILD_ROOT/talloc-${TALLOC_VERSION}"
./configure --prefix="$BUILD_ROOT/talloc" >"$BUILD_ROOT/talloc-config.log" 2>&1 || true
test -f bin/default/config.h || {
	echo "talloc configure did not generate bin/default/config.h" >&2
	exit 1
}
mkdir -p "$BUILD_ROOT/talloc/include" "$BUILD_ROOT/talloc/lib"
gcc -c -I. -Ilib/replace -Ibin/default -D__STDC_WANT_LIB_EXT1__=1 -O2 \
	talloc.c -o "$BUILD_ROOT/talloc.o"
gcc -c -I. -Ilib/replace -Ibin/default -D__STDC_WANT_LIB_EXT1__=1 -O2 \
	lib/replace/replace.c -o "$BUILD_ROOT/replace.o"
ar rcs "$BUILD_ROOT/talloc/lib/libtalloc.a" "$BUILD_ROOT/talloc.o" "$BUILD_ROOT/replace.o"
cp talloc.h "$BUILD_ROOT/talloc/include/"
gcc -static -O2 "$HERE/gate_unix_socket.c" -o "$BUILD_ROOT/gate-unix-socket"
gcc -static -O2 "$HERE/gate_path_resolution.c" -o "$BUILD_ROOT/gate-path-resolution"

check_path_resolution() {
	local proot="$1" guest="/gpuworks-proot-path-$$" source="$BUILD_ROOT/path-source"
	test ! -e "$guest"
	mkdir -p "$source/data/leaf"
	test -L "$source/link" || ln -s data "$source/link"
	"$proot" -0 -k 6.8.0 -r / -b "$source:$guest" -w / \
	 "$BUILD_ROOT/gate-path-resolution" "$guest" | grep -qx 'path_resolution=pass'
}

check_unix_socket_isolation() {
	local proot="$1" name="$2" gate_root ready host_socket guest_socket pid isolated=false
	# Docker Desktop's linux/amd64 Rosetta path does not expose ptrace/seccomp
	# behavior equivalent to a native Linux kernel. Refuse to certify isolation
	# there instead of misclassifying both variants.
	if test -x /run/rosetta/rosetta; then
		return 2
	fi
	gate_root="$BUILD_ROOT/gate-root-$name"
	ready="$BUILD_ROOT/gate-ready-$name"
	host_socket="/tmp/gpuworks-proot-gate-$$-$name.sock"
	guest_socket="$gate_root$host_socket"
	mkdir -p "$gate_root/tmp"
	test ! -e "$host_socket"
	"$proot" -0 -k 6.8.0 -r "$gate_root" -b "$BUILD_ROOT:$BUILD_ROOT" -w / \
		"$BUILD_ROOT/gate-unix-socket" "$host_socket" "$ready" &
	pid=$!
	for _ in 1 2 3 4 5; do
		test -f "$ready" && break
		sleep 1
	done
	# A long translated path may be represented by PRoot through its own short
	# temporary binding rather than as gate_root + guest path. Successful bind +
	# readiness and absence of the exact host path is the isolation invariant.
	if test -f "$ready" && test ! -e "$host_socket"; then
		isolated=true
	fi
	kill "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	rm -f "$host_socket" "$guest_socket" "$ready"
	test "$isolated" = true
}

build_variant() {
	local name="$1" patch_file output tree manifest isolation gate_status
	case "$name" in
		nofd) patch_file="$PATCH_DIR/proot-nobrk-nofd.patch" ;;
		nosocket) patch_file="$PATCH_DIR/proot-nosocket.patch" ;;
	esac
	if [ -n "${PROOT_OUTPUT:-}" ]; then
		output="$PROOT_OUTPUT"
	else
		output="$OUTPUT_DIR/proot-${name}-static"
	fi
	tree="$BUILD_ROOT/proot-$name"
	manifest="$output.manifest"

	git -C "$BUILD_ROOT/proot-source" worktree add -q --detach "$tree" "$PROOT_COMMIT"
	cd "$tree"
	patch --batch --forward -p1 < "$patch_file"
	patch --batch --forward -p1 < "$PATCH_DIR/proot-temp-noise.patch"
	patch --batch --forward -p1 < "$PATCH_DIR/proot-arm64-faccessat2.patch"
	make -C src clean >/dev/null 2>&1 || true
	PKG_CONFIG_PATH="$BUILD_ROOT/talloc/lib/pkgconfig" make -C src \
		CFLAGS="-O2 -I$BUILD_ROOT/talloc/include" \
		LDFLAGS="-static -Wl,-z,noexecstack -L$BUILD_ROOT/talloc/lib -ltalloc"

	cp src/proot "$output"
	file "$output"
	# Compatibility gate matching udocker's production invocation. The old
	# udocker-1-derived binaries failed here with libm.so.6 EINVAL when -k was
	# present, before vLLM or NCCL could start.
	"$output" -0 -k 6.8.0 -r / \
		-b /proc:/proc -b /sys:/sys -b /dev:/dev -b /etc/hosts:/etc/hosts \
		-w / /usr/bin/python3 -c 'import math; print("ok")' | grep -qx ok

	check_path_resolution "$output"
	if check_unix_socket_isolation "$output" "$name"; then
		isolation=pass
	else
		gate_status=$?
		if [ "$gate_status" -eq 2 ]; then
			isolation=not-run-emulated
		else
			isolation=fail
		fi
	fi
	{
		echo "variant=$name"
		echo "proot_repository=$PROOT_REPOSITORY"
		echo "proot_commit=$PROOT_COMMIT"
		echo "talloc_version=$TALLOC_VERSION"
		echo "talloc_sha256=$TALLOC_SHA256"
		echo "variant_patch_sha256=$(sha256sum "$patch_file" | awk '{print $1}')"
		echo "temp_patch_sha256=$(sha256sum "$PATCH_DIR/proot-temp-noise.patch" | awk '{print $1}')"
		echo "arm64_faccessat2_patch_sha256=$(sha256sum "$PATCH_DIR/proot-arm64-faccessat2.patch" | awk '{print $1}')"
		echo "path_resolution_gate=pass"
		echo "compiler=$(gcc --version | head -n1)"
		echo "python_k_6_8_gate=pass"
		echo "af_unix_path_isolation=$isolation"
		echo "artifact_sha256=$(sha256sum "$output" | awk '{print $1}')"
		file "$output"
	} > "$manifest"
	cat "$manifest"
	if [ "$isolation" != pass ] && [ "${PROOT_ALLOW_UNSAFE_NOSOCKET:-}" != 1 ]; then
		if [ "$isolation" = not-run-emulated ]; then
			echo "$name cannot be certified under Docker Desktop/Rosetta; rerun on native Linux amd64" >&2
		else
			echo "$name failed AF_UNIX path-isolation gate; artifact is unsafe for rootless containers" >&2
		fi
		echo "set PROOT_ALLOW_UNSAFE_NOSOCKET=1 only to retain an uncertified artifact for isolated research" >&2
		exit 1
	fi
}

case "$VARIANT" in
	both) build_variant nofd; build_variant nosocket ;;
	*) build_variant "$VARIANT" ;;
esac
