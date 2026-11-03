#!/bin/bash
# Rebuild the embedded internal/assets/bin/proot-x86_64-static-nofd byte for
# byte from its corresponding source, and fail unless the result is identical.
#
# The shipped x86_64 binary predates build.sh and was built differently, so
# build.sh cannot reproduce it. This script pins every input that shows up in
# the bytes:
#
#   - PRoot: upstream proot-me/proot 704a4ab7 (its src/ equals 5f780cba5
#     "Add clone3"), checked out shallow and tagless, with git installed, so
#     the version banner reads "-704a4ab7"; patched with
#     patches/proot-nobrk-nofd.patch only.
#   - Toolchain: Ubuntu 24.04 gcc 13.3.0-6ubuntu2~24.04.1, with glibc
#     2.39-0ubuntu8.8 (libc.a is linked in statically; 8.9 and earlier 8.x
#     builds differ). That glibc is no longer in the archive, so its packages
#     are fetched from Launchpad and checked against pinned sha256 sums.
#   - talloc 2.4.2 built by its own waf (default flags, no -O), objects
#     archived into a private libtalloc.a.
#   - PRoot compiled with CFLAGS="-Wall -Wextra" and LDFLAGS="-static ...":
#     no -O, no -g, not stripped, and no -Wl,-z,noexecstack.
#
# Run it in a native linux/amd64 Ubuntu 24.04 container:
#
#   out="$(mktemp -d)"
#   docker run --rm --platform linux/amd64 -v "$PWD:/src:ro" -v "$out:/out" \
#     ubuntu:24.04 env PROOT_OUTPUT_DIR=/out \
#     bash /src/build/runtime-assets/proot/reproduce-embedded-x86_64.sh
#
# New builds use build.sh; this script only documents and proves the source of
# the binary that is shipped today.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../../.." && pwd)"
EMBEDDED="$REPO_ROOT/internal/assets/bin/proot-x86_64-static-nofd"
EXPECTED_SHA256=4ea60176380afa39024d9e5e12e34cae959164395ceb71761d45383b393a7d56

PROOT_REPOSITORY=https://github.com/proot-me/proot.git
PROOT_COMMIT=704a4ab7a41cf5a8bc1f66b0ebdc58cafa72d88c
TALLOC_VERSION=2.4.2
TALLOC_URL="https://download.samba.org/pub/talloc/talloc-${TALLOC_VERSION}.tar.gz"
TALLOC_SHA256=85ecf9e465e20f98f9950a52e9a411e14320bc555fa257d87697b7e7a9b1d8a6
GCC_VERSION=13.3.0-6ubuntu2~24.04.1
GLIBC_VERSION=2.39-0ubuntu8.8
GLIBC_URL=https://launchpad.net/ubuntu/+archive/primary/+files
GLIBC_DEBS="
3b8d5391b6b484a4c81fd000b6064885ad967ec3cb966bc57603f3fb3ebf0ed5 libc6_${GLIBC_VERSION}_amd64.deb
bb8741966e7c1d2e2c0b84bb311717a0908fb563d9b2247b7212710e3cd88b94 libc6-dev_${GLIBC_VERSION}_amd64.deb
c894e5a5f137429657d09e853fbbb19d53fc164c60804c396cd43873b0b4f734 libc-dev-bin_${GLIBC_VERSION}_amd64.deb
"
OUTPUT_DIR="${PROOT_OUTPUT_DIR:-$HERE}"
BUILD_ROOT="$(mktemp -d /tmp/gpuworks-proot-reproduce.XXXXXX)"
trap 'rm -rf "$BUILD_ROOT"' EXIT

if [ "$(uname -m)" != x86_64 ]; then
	echo "run this on linux/amd64; got $(uname -m)" >&2
	exit 2
fi
mkdir -p "$OUTPUT_DIR"

apt-get update -q
apt-get install -y -q ca-certificates curl git make patch python3 \
	"gcc-13=$GCC_VERSION" "cpp-13=$GCC_VERSION" "gcc-13-base=$GCC_VERSION" \
	"libgcc-13-dev=$GCC_VERSION" gcc

mkdir -p "$BUILD_ROOT/debs"
while read -r sum deb; do
	[ -n "$deb" ] || continue
	curl --fail --location --silent --show-error "$GLIBC_URL/$deb" -o "$BUILD_ROOT/debs/$deb"
	echo "$sum  $BUILD_ROOT/debs/$deb" | sha256sum --check --status
done <<<"$GLIBC_DEBS"
apt-get install -y -q --allow-downgrades "$BUILD_ROOT"/debs/*.deb
dpkg-query -W -f='${Version}\n' libc6-dev | grep -qx "$GLIBC_VERSION"
gcc --version | head -1

# A shallow, tagless checkout: GNUmakefile derives the version banner from
# `git describe --tags` (empty here) and the newest commit in the clone.
git init -q "$BUILD_ROOT/proot-source"
git -C "$BUILD_ROOT/proot-source" fetch -q --depth 1 --no-tags "$PROOT_REPOSITORY" "$PROOT_COMMIT"
git -C "$BUILD_ROOT/proot-source" checkout -q FETCH_HEAD
patch -d "$BUILD_ROOT/proot-source" -p1 <"$HERE/patches/proot-nobrk-nofd.patch"

curl --fail --location --silent --show-error "$TALLOC_URL" -o "$BUILD_ROOT/talloc.tar.gz"
echo "$TALLOC_SHA256  $BUILD_ROOT/talloc.tar.gz" | sha256sum --check --status
tar xzf "$BUILD_ROOT/talloc.tar.gz" -C "$BUILD_ROOT"
(
	cd "$BUILD_ROOT/talloc-${TALLOC_VERSION}"
	./configure --disable-python >"$BUILD_ROOT/talloc-configure.log" 2>&1
	make >"$BUILD_ROOT/talloc-make.log" 2>&1
)
mkdir -p "$BUILD_ROOT/talloc/include" "$BUILD_ROOT/talloc/lib"
ar rcs "$BUILD_ROOT/talloc/lib/libtalloc.a" \
	"$BUILD_ROOT/talloc-${TALLOC_VERSION}/bin/default/talloc.c.6.o" \
	"$BUILD_ROOT/talloc-${TALLOC_VERSION}/bin/default/lib/replace/replace.c.1.o"
cp "$BUILD_ROOT/talloc-${TALLOC_VERSION}/talloc.h" "$BUILD_ROOT/talloc/include/"

make -C "$BUILD_ROOT/proot-source/src" -j"$(nproc)" proot \
	CFLAGS="-Wall -Wextra -I$BUILD_ROOT/talloc/include" \
	LDFLAGS="-static -L$BUILD_ROOT/talloc/lib -ltalloc" >"$BUILD_ROOT/proot-make.log" 2>&1

output="$OUTPUT_DIR/proot-x86_64-static-nofd.reproduced"
cp "$BUILD_ROOT/proot-source/src/proot" "$output"
actual="$(sha256sum "$output" | cut -d' ' -f1)"
echo "reproduced: $output"
echo "sha256:     $actual"
if [ "$actual" != "$EXPECTED_SHA256" ]; then
	echo "mismatch: expected $EXPECTED_SHA256" >&2
	exit 1
fi
if [ -f "$EMBEDDED" ] && ! cmp -s "$output" "$EMBEDDED"; then
	echo "the reproduced binary differs from $EMBEDDED" >&2
	exit 1
fi
echo "byte-identical to the embedded proot-x86_64-static-nofd"
