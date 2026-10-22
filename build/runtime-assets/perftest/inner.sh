#!/bin/bash
# Runs inside the Debian 10 build container; see build.sh.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

printf '%s\n' \
	"deb $APT_MIRROR/debian buster main" \
	"deb $APT_MIRROR/debian buster-updates main" \
	"deb $APT_MIRROR/debian-security buster/updates main" > /etc/apt/sources.list
echo 'Acquire::Check-Valid-Until "false";' > /etc/apt/apt.conf.d/99archive

if [ "$GOARCH" = arm64 ]; then
	dpkg --add-architecture arm64
	triplet=aarch64-linux-gnu
	pkgs="crossbuild-essential-arm64 libnl-3-dev:arm64 libnl-route-3-dev:arm64 libpci-dev:arm64 libudev-dev:arm64"
	host="--host=$triplet"
	cmake_cross="-DCMAKE_C_COMPILER=$triplet-gcc -DCMAKE_SYSTEM_NAME=Linux -DCMAKE_SYSTEM_PROCESSOR=aarch64 -DCMAKE_FIND_ROOT_PATH=/usr/$triplet"
	tool="$triplet-"
else
	triplet=x86_64-linux-gnu
	pkgs="libnl-3-dev libnl-route-3-dev libpci-dev libudev-dev"
	host=""; cmake_cross=""; tool=""
fi
apt-get update -q >/dev/null
apt-get install -y -q --no-install-recommends build-essential autoconf automake libtool pkg-config cmake ninja-build python3 file patch $pkgs >/tmp/apt.log 2>&1 || { tail -20 /tmp/apt.log; exit 1; }

# rdma-core: only its headers and link-time libraries are used. At run time
# the host's own libibverbs and librdmacm load instead, so this version is
# the oldest the host may have (the symbol versions linked in come from it).
prefix=/opt/rdma-core
mkdir -p /build && tar xzf /work/rdma-core.tar.gz -C /build
cd /build/rdma-core-*
mkdir build && cd build
PKG_CONFIG_PATH="/usr/lib/$triplet/pkgconfig" cmake -GNinja $cmake_cross \
	-DCMAKE_INSTALL_PREFIX=$prefix -DNO_MAN_PAGES=1 -DNO_PYVERBS=1 -DENABLE_RESOLVE_NEIGH=0 \
	-DENABLE_STATIC=0 -DIOCTL_MODE=both .. >/build/rdma-core-cmake.log 2>&1 || { tail -30 /build/rdma-core-cmake.log; exit 1; }
ninja >/build/rdma-core-ninja.log 2>&1 || { grep -m20 error /build/rdma-core-ninja.log; tail -20 /build/rdma-core-ninja.log; exit 1; }
ninja install >/dev/null
# perftest turns vendor features on from these headers alone; without them it
# builds generic verbs only (see the configure note below).
rm -f $prefix/include/infiniband/mlx4dv.h $prefix/include/infiniband/mlx5*.h \
	$prefix/include/infiniband/hnsdv.h $prefix/include/infiniband/efadv.h

# perftest: only ib_write_bw, without the vendor direct-verbs libraries
# (libmlx5, libhns, libefa) so it loads on any host with libibverbs and
# librdmacm; generic verbs is all the bandwidth test needs. The ibv_wr_* send
# API is off: hns (Huawei) providers of rdma-core 35 cannot create its QPs
# ("Unable to create QP"), and the classic post-send path measures the same
# bandwidth (24.5 Gb/s on a 25G hns rail either way).
mkdir -p /build/perftest && tar xzf /work/perftest.tar.gz -C /build/perftest --strip-components=1
cd /build/perftest
for p in /work/patches/*.patch; do patch -p1 --quiet < "$p"; done
./autogen.sh >/dev/null 2>&1
./configure $host CPPFLAGS="-I$prefix/include" LDFLAGS="-L$prefix/lib" \
	--disable-cudart --disable-rocm --disable-neuron --disable-hl --disable-mlu --disable-ibv_wr_api \
	ac_cv_lib_mlx5_mlx5dv_create_qp=no ac_cv_lib_hns_hnsdv_query_device=no \
	ac_cv_lib_efa_efadv_create_qp_ex=no ac_cv_lib_efa_efadv_create_driver_qp=no >/build/configure.log 2>&1 || { tail -30 /build/configure.log; exit 1; }
make -j"$(nproc)" ib_write_bw >/build/make.log 2>&1 || { grep -B2 -A8 -m5 -E "undefined reference|cannot find|error:" /build/make.log | tail -40; exit 1; }
mkdir -p /work/out
${tool}strip -o /work/out/ib_write_bw ib_write_bw
echo "built $(file -b /work/out/ib_write_bw | cut -c1-80)"
echo "NEEDED: $(${tool}objdump -p /work/out/ib_write_bw | awk '/NEEDED/{print $2}' | tr '\n' ' ')"
echo "symbol versions: $(${tool}objdump -T /work/out/ib_write_bw | grep -o -E '(GLIBC|IBVERBS|RDMACM)_[0-9.]+' | sort -V -u | tr '\n' ' ')"
