FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
RUN apt-get update && apt-get install -y --no-install-recommends nfs-ganesha nfs-ganesha-vfs iproute2 && rm -rf /var/lib/apt/lists/* && mkdir -p /run/ganesha /var/lib/nfs/ganesha /export
ENTRYPOINT ["/bin/sh", "-c", "mount -o rw /dev/nfs-export /export && rpcbind && exec /usr/bin/ganesha.nfsd -F -L /dev/stdout -f /etc/ganesha/ganesha.conf"]
