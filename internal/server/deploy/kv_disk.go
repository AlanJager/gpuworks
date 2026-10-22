// kv_disk.go owns the one launch fact the KV disk tier adds beyond argv: the
// container path the tier writes to has to be a real host directory the agent
// can watch and prune, so the launch carries a managed mount for it.

package deploy

import (
	"path"
	"strconv"

	"gpuworks.ai/gpuworks/internal/proto"
)

// KVDiskMinKernel is the lowest Linux release the vLLM KV disk tier can run
// on. The tier's shared offload region is populated with
// madvise(MADV_POPULATE_WRITE), an advice the kernel only gained in 5.14 and
// rejects with EINVAL before that, so the engine dies while creating the
// region instead of serving (measured on a 4.18 el8 host with vLLM 0.25.1,
// which has no EINVAL fallback; see
// artifacts/archive/2026-09-16-kv-disk-offload/acceptance/nvidia-madvise-einval.md). It is a
// property of the engine, published on its profile as kv_disk_min_kernel, and
// clients compare it against the kernel a node reports.
const KVDiskMinKernel = "5.14"

// kvDiskLimitBytes is the ceiling the agent keeps the disk tier directory
// under. A stated value wins; otherwise it is twice the model's weight bytes -
// a derived hint that travels with the launch and is never written back into
// the deployment's configuration (ADR 0020 decision 3). Zero means the platform
// has no ceiling to enforce, which is also what a preview without a sized model
// reports.
func kvDiskLimitBytes(q LaunchConfig, weightBytes int64) int64 {
	if q.KVDiskLimitGiB > 0 {
		return int64(q.KVDiskLimitGiB * float64(int64(1)<<30))
	}
	if weightBytes > 0 {
		return weightBytes * 2
	}
	return 0
}

// kvDiskMount is the managed mount for the disk tier's root directory, or false
// when the deployment names no directory. Source is deliberately empty: which
// host directory backs it is the agent's decision, and the server does not
// guess a node's layout. SizeHint carries the ceiling in bytes.
func kvDiskMount(q LaunchConfig, weightBytes int64) (proto.MountSpec, bool) {
	if q.SharedKVDir == "" {
		return proto.MountSpec{}, false
	}
	mount := proto.MountSpec{Target: path.Clean(q.SharedKVDir), Purpose: proto.MountPurposeKVDisk}
	if limit := kvDiskLimitBytes(q, weightBytes); limit > 0 {
		mount.SizeHint = strconv.FormatInt(limit, 10)
	}
	return mount, true
}
