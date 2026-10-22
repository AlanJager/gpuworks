#!/bin/sh
# gpuworks-nvidia-kmod: load the NVIDIA kernel modules this machine's GPUs
# can run. The golden image carries both builds of one driver version:
# the open modules (required by Blackwell, e.g. RTX 5090) and the
# proprietary ones (required by GPUs without a GSP, e.g. V100S, P40).
# Open is tried first because every card that worked before this image
# used it; when it initialises no GPU, the proprietary build is loaded.
set -u
store="${GPUWORKS_NVIDIA_KMOD_STORE:-@STORE@}"
dest="${GPUWORKS_NVIDIA_KMOD_DEST:-/lib/modules/$(uname -r)/updates/gpuworks-nvidia}"
state="${GPUWORKS_NVIDIA_KMOD_STATE:-/run/gpuworks-nvidia-kmod}"
gpus="${GPUWORKS_NVIDIA_GPUS_DIR:-/proc/driver/nvidia/gpus}"

log() { echo "gpuworks-nvidia-kmod: $*"; }

unload() {
	for module in nvidia_peermem nvidia_drm nvidia_modeset nvidia_uvm nvidia; do
		rmmod "$module" 2>/dev/null
	done
}

load() {
	unload
	rm -rf "$dest"
	mkdir -p "$dest"
	cp "$store/$1"/*.ko "$dest/" || return 1
	depmod -a
	modprobe nvidia || return 1
	# Only a GPU the driver initialised gets an entry here.
	[ -n "$(ls "$gpus" 2>/dev/null)" ]
}

if ! lspci -n -d 10de: 2>/dev/null | grep -q .; then
	log "no NVIDIA device"
	echo none >"$state"
	exit 0
fi
for flavour in open proprietary; do
	if load "$flavour"; then
		modprobe nvidia_uvm
		modprobe nvidia_modeset
		nvidia-modprobe -u -c=0 2>/dev/null
		echo "$flavour" >"$state"
		log "loaded $flavour modules"
		exit 0
	fi
	log "$flavour modules initialised no GPU"
done
unload
echo failed >"$state"
log "neither module build initialised a GPU"
exit 1
