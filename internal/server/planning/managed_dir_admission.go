// managed_dir_admission.go admits the writable directories a template declares
// (spec dirs, ADR 0077) on the disk the node keeps them on.

package planning

import (
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// storagePolicyReader is the one read managed directory admission makes:
// the storage settings' disk reserve. *store.Store satisfies it.
type storagePolicyReader interface {
	StoragePolicy() store.StoragePolicy
}

// managedDirNeed is the admission of one launch's managed directories on one
// node: the free space the disk reports, what the launch needs there, and the
// parts that need is made of (for the refusal's params).
type managedDirNeed struct {
	AvailBytes   int64
	NeedBytes    int64
	DirBytes     int64
	ReserveBytes int64
	ImageBytes   int64
	WeightBytes  int64
	Path         string
}

// managedDirAdmission works out what the disk holding managed directories must
// have free: the declared limits plus the disk reserve (the storage settings'
// value, else store.DefaultReserveFreeBytes of that disk), plus the image to
// unpack and the weights to download when the agent reports they land on the
// same filesystem (proto.PathUsage.SharesDiskWith). The weights are charged
// whenever the managed disk shares the models root's disk, even when the
// deployment's storage keeps them elsewhere: the over-count errs on the side of
// a full disk not happening. A launch without directories, or a node that does
// not report the disk (an older agent), needs nothing here.
func managedDirAdmission(policy storagePolicyReader, node *store.Node, dirBytes, weightNeedBytes, imageNeedBytes int64) managedDirNeed {
	if dirBytes <= 0 {
		return managedDirNeed{}
	}
	usage, ok := nodePathUsage(node, proto.PathKindManagedDirs, 0)
	if !ok || usage.AvailableBytes <= 0 {
		return managedDirNeed{DirBytes: dirBytes}
	}
	need := managedDirNeed{AvailBytes: usage.AvailableBytes, DirBytes: dirBytes, Path: usage.Path}
	need.ReserveBytes = policy.StoragePolicy().ReserveFreeBytes
	if need.ReserveBytes <= 0 {
		need.ReserveBytes = store.DefaultReserveFreeBytes(usage.TotalBytes)
	}
	if slices.Contains(usage.SharesDiskWith, "models") && weightNeedBytes > 0 {
		need.WeightBytes = weightNeedBytes
	}
	if runtime := strings.ToLower(strings.TrimSpace(node.RuntimeDriver)); runtime != "" && slices.Contains(usage.SharesDiskWith, runtime) && imageNeedBytes > 0 {
		need.ImageBytes = imageNeedBytes
	}
	need.NeedBytes = need.DirBytes + need.ReserveBytes + need.WeightBytes + need.ImageBytes
	return need
}

// explainManagedDirRefusal states the figures of a managed directory refusal:
// what the disk has, what the launch needs and of what.
func explainManagedDirRefusal(in Input, need managedDirNeed, result *scheduler.Result) {
	if result.Message.Code != scheduler.ReasonInsufficientManagedDirDisk {
		return
	}
	params := map[string]any{
		"required_bytes": need.NeedBytes, "available_bytes": need.AvailBytes,
		"dir_bytes": need.DirBytes, "reserve_bytes": need.ReserveBytes,
		"image_bytes": need.ImageBytes, "weight_bytes": need.WeightBytes,
	}
	if strings.HasPrefix(need.Path, "/") {
		params["path"] = need.Path
	}
	if in.Node != nil {
		params["node"], params["node_uuid"] = in.Node.DisplayName(), in.Node.UUID
	}
	result.Message.Params = params
	for index := range result.Explain.Filters {
		if result.Explain.Filters[index].Code == result.Message.Code {
			result.Explain.Filters[index].Params = params
		}
	}
}
