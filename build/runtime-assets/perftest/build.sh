#!/bin/bash
# Build the ib_write_bw embedded in the agent: upstream perftest linked
# against a pinned rdma-core, in Debian 10 (glibc 2.28), for one architecture.
# See README.md for why each choice. Run on a Docker host:
#   GOARCH=amd64 build/runtime-assets/perftest/build.sh
#   GOARCH=arm64 build/runtime-assets/perftest/build.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
GOARCH="${GOARCH:-amd64}"
PERFTEST_VERSION=26.07.8
PERFTEST_URL="https://github.com/linux-rdma/perftest/archive/refs/tags/${PERFTEST_VERSION}.tar.gz"
PERFTEST_SHA256=4d348ce6a36886b32da0afc9fce49b9f4350674cdc18ae1f49343907d1b1afaa
RDMA_CORE_VERSION=35.0
RDMA_CORE_URL="https://github.com/linux-rdma/rdma-core/releases/download/v${RDMA_CORE_VERSION}/rdma-core-${RDMA_CORE_VERSION}.tar.gz"
RDMA_CORE_SHA256=c87d4faa8bd88c485c59faa34a8a323a2ba2dd765b007e28c4abf6d333526372
BUILD_IMAGE="${PERFTEST_BUILD_IMAGE:-debian:buster}"
APT_MIRROR="${PERFTEST_APT_MIRROR:-http://archive.debian.org}"
OUTPUT_DIR="${PERFTEST_OUTPUT_DIR:-$REPO/internal/assets/bin}"

case "$GOARCH" in
	amd64) asset=ib_write_bw-x86_64 ;;
	arm64) asset=ib_write_bw-arm64 ;;
	*) echo "GOARCH must be amd64 or arm64" >&2; exit 2 ;;
esac

work="$(mktemp -d /tmp/gpuworks-perftest.XXXXXX)"
trap 'rm -rf "$work"' EXIT
# PERFTEST_SOURCE_DIR may hold the tarballs already (named as in the URLs'
# last path element); they are checked against the same pinned SHA-256.
fetch() {
	local cached="${PERFTEST_SOURCE_DIR:-}/$4"
	if [ -n "${PERFTEST_SOURCE_DIR:-}" ] && [ -f "$cached" ]; then
		cp "$cached" "$2"
	else
		curl --fail --location --silent --show-error --retry 3 "$1" -o "$2"
	fi
	echo "$3  $2" | sha256sum --check --status || { echo "checksum mismatch: $1" >&2; exit 1; }
}
fetch "$PERFTEST_URL" "$work/perftest.tar.gz" "$PERFTEST_SHA256" "perftest-${PERFTEST_VERSION}.tar.gz"
fetch "$RDMA_CORE_URL" "$work/rdma-core.tar.gz" "$RDMA_CORE_SHA256" "rdma-core-${RDMA_CORE_VERSION}.tar.gz"
cp "$HERE/inner.sh" "$work/"
cp -r "$HERE/patches" "$work/"

docker run --rm -e GOARCH="$GOARCH" -e APT_MIRROR="$APT_MIRROR" -v "$work:/work" "$BUILD_IMAGE" bash /work/inner.sh
install -m 0755 "$work/out/ib_write_bw" "$OUTPUT_DIR/$asset"
echo "$(sha256sum "$OUTPUT_DIR/$asset" | cut -d' ' -f1)  $asset"
