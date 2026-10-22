# syntax=docker/dockerfile:1

# Keep the probe runtime independent from the builder host. In particular,
# Ascend nodes can have an old host glibc even though libvruntime requires
# GLIBC_2.33/2.34 inside the workload container.
#
# Docker Hub is intermittently unreachable from the build host, so pull the
# same content-addressed image through the mainland mirror instead. The pinned
# digest guarantees byte-identical content regardless of which registry serves
# it.
ARG GPUWORKS_PROBE_BASE_IMAGE=tx0axs7hravg1ta0ch.xuanyuan.run/library/ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517
FROM ${GPUWORKS_PROBE_BASE_IMAGE} AS build

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install --no-install-recommends -y gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY probe.c ./probe.c
RUN cc -O2 -Wall -Wextra probe.c -ldl -o /probe

# libvruntime and vendor driver libraries are injected only when the probe is
# run, so ldd(1) on /probe cannot discover their compatibility dependencies.
# Copy the small glibc compatibility DSOs explicitly as well as /probe's own
# closure. On glibc >= 2.34 several of these are compatibility stubs, but their
# SONAMEs are still required by vendor libraries built against older releases.
RUN set -eu; \
    mkdir -p /probe-rootfs; \
    copy_library() { \
        name="$1"; \
        path="$(ldconfig -p | awk -v name="${name}" '$1 == name { print $NF; exit }')"; \
        test -n "${path}"; \
        mkdir -p "/probe-rootfs$(dirname "${path}")"; \
        cp -L "${path}" "/probe-rootfs${path}"; \
    }; \
    cp /probe /probe-rootfs/probe; \
    for name in libc.so.6 libpthread.so.0 libdl.so.2 libm.so.6 librt.so.1; do \
        copy_library "${name}"; \
    done; \
    loader="$(ldd /probe | awk '/ld-linux|ld-musl/ { for (i=1;i<=NF;i++) if ($i ~ /^\//) { print $i; exit } }')"; \
    test -n "${loader}"; \
    mkdir -p "/probe-rootfs$(dirname "${loader}")"; \
    cp -L "${loader}" "/probe-rootfs${loader}"

FROM scratch
COPY --from=build /probe-rootfs /
ENTRYPOINT ["/probe"]
