# The agent image a Kubernetes cluster's DaemonSet runs (internal/server/
# kubemanifest). It is the ordinary static agent plus what the kube runtime
# shells out to on the node: nerdctl for images in kubelet's containerd
# namespace, mount.nfs for NFS storages. The base is glibc-based because the
# NVIDIA runtime mounts the host's nvidia-smi, a glibc program, into the
# agent's container.
FROM debian:bookworm-slim
ARG DEBIAN_MIRROR=deb.debian.org
RUN sed -i "s#deb.debian.org#${DEBIAN_MIRROR}#g" /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends nfs-common ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY nerdctl /usr/local/bin/nerdctl
COPY gpuworks-agent /usr/local/bin/gpuworks-agent
ENTRYPOINT ["/usr/local/bin/gpuworks-agent"]
