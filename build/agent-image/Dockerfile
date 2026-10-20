# The agent image a Kubernetes cluster's DaemonSet runs (internal/server/
# kubemanifest): the ordinary static agent plus nerdctl, which the kube
# runtime installs into the node and runs there, in the host's mount
# namespace (internal/agent/runtime/kube/host.go). NFS storages are mounted
# the same way, with the node's own mount.nfs. The base is glibc-based
# because the NVIDIA runtime mounts the host's nvidia-smi, a glibc program,
# into the agent's container, and Ascend's npu-smi is mounted from the host.
#
# The final stage only copies, so the image builds for any architecture
# without emulation; the CA bundle comes from a stage on the build platform.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS certs
ARG DEBIAN_MIRROR=deb.debian.org
RUN sed -i "s#deb.debian.org#${DEBIAN_MIRROR}#g" /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates

FROM debian:bookworm-slim
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY nerdctl gpuworks-agent /usr/bin/
ENTRYPOINT ["/usr/bin/gpuworks-agent"]
