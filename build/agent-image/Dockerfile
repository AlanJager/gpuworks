# The agent image a Kubernetes cluster's DaemonSet runs (internal/server/
# kubemanifest). It is the ordinary static agent plus what the kube runtime
# shells out to on the node: nerdctl for images in kubelet's containerd
# namespace, mount.nfs for NFS storages.
FROM alpine:3.20
ARG ALPINE_MIRROR=https://dl-cdn.alpinelinux.org/alpine
RUN sed -i "s#https\?://dl-cdn.alpinelinux.org/alpine#${ALPINE_MIRROR}#" /etc/apk/repositories \
    && apk add --no-cache nfs-utils util-linux ca-certificates
COPY nerdctl /usr/local/bin/nerdctl
COPY gpuworks-agent /usr/local/bin/gpuworks-agent
ENTRYPOINT ["/usr/local/bin/gpuworks-agent"]
