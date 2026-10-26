package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Preparation phases beyond the agent's copy phases (proto.ModelPhase*) and
// the controller's eviction phases (prepobs.Phase*).
const (
	PreparationWaiting     = "waiting"
	PreparationReady       = "ready"
	PreparationDownloading = "downloading"
	PreparationSyncing     = "syncing"
)

// Preparation reasons: stable codes the UI localizes.
const (
	PreparationReasonWaiting             = "MODEL_PREPARATION_WAITING"
	PreparationReasonNodeOffline         = "MODEL_NODE_OFFLINE"
	PreparationReasonCacheDiskFull       = "MODEL_CACHE_DISK_FULL"
	PreparationReasonFailed              = "MODEL_PREPARATION_FAILED"
	PreparationReasonStorageUnresponsive = "MODEL_STORAGE_UNRESPONSIVE"
	PreparationReasonEvictionFailed      = "MODEL_CACHE_EVICTION_FAILED"
	PreparationReasonCopyStalled         = "MODEL_COPY_STALLED"
)

// ModelPreparation is a pure reduction of authoritative facts. It is not a
// launch condition: only ModelLocation.Ready admits an instance.
type ModelPreparation struct {
	Phase       string
	Reason      string
	TotalKnown  bool
	IdleSeconds int64
	ETASeconds  int64
	// Retryable: the row is a failed cache copy an operator may retry
	// (CacheCopyRetryable).
	Retryable bool
	// Observed: the bytes and rate shown come from the agent's live copy
	// observation rather than the row.
	Observed bool
	// RateShown: a transfer rate is meaningful right now.
	RateShown bool
}

// CacheCopyRetryable is whether a cache row is a failed same-node copy whose
// intent a retry may replace. The API's offer and the controller's check are
// both this predicate.
func CacheCopyRetryable(storage *store.Storage, file *store.ModelFile) bool {
	return storage != nil && file != nil && storage.Type == proto.StorageNFSCache &&
		file.Acquisition == proto.ModelAcquireCopy && file.TransferID != "" &&
		(file.State == proto.MFFailed || file.State == proto.MFInsufficientDisk)
}

// ModelPreparationFile is the transfer a deployment is waiting for. Before
// a shared model is complete, the shared row holds that transfer, even when
// an nfs_cache storage retains an older node-cache row. Once it is complete,
// preparation follows the node's own cache copy instead. Inactive missing
// rows do not represent a transfer; only a shared transfer failure is retained
// when the source refuses to proceed.
func ModelPreparationFile(source store.ModelSource) *store.ModelFile {
	if shared := source.Location.Shared; shared != nil {
		if source.Action == store.SourceWaitShared {
			return shared
		}
		if source.Location.Reason == store.LocationModelMissingInStore {
			switch shared.State {
			case proto.MFFailed, proto.MFDownloadRequired, proto.MFInsufficientDisk:
				return shared
			}
		}
	}
	return source.Location.File
}

// PrepareModelView reduces a node's source for one model, the agent's latest
// copy observation (nil when none is fresh) and the controller's cache phase
// to what the deployment shows.
func PrepareModelView(node *store.Node, source store.ModelSource, observation *proto.ModelFileStatus, cachePhase string) ModelPreparation {
	if source.Location.Ready {
		return ModelPreparation{Phase: PreparationReady}
	}
	view := ModelPreparation{Phase: PreparationWaiting, Reason: PreparationReasonWaiting}
	file := ModelPreparationFile(source)
	// A download or peer sync reports its bytes on the row itself.
	rowTransfer := file != nil && (file.Acquisition == proto.ModelAcquireDownload || file.Acquisition == proto.ModelAcquirePeer)
	if node.State == store.NodeStateOffline {
		view.Reason = PreparationReasonNodeOffline
		return view
	}
	view.TotalKnown = rowTransfer && file.SizeBytes > 0
	if file != nil {
		view.Retryable = file == source.Location.File && CacheCopyRetryable(&source.Location.Storage, file)
		switch file.State {
		case proto.MFInsufficientDisk:
			view.Reason = PreparationReasonCacheDiskFull
			return view
		case proto.MFDeleting:
			view.Reason = PreparationReasonEvictionFailed
			return view
		case proto.MFFailed:
			view.Reason = PreparationReasonFailed
			return view
		}
	}
	if state := source.Location.NodeState; state != nil && state.State == proto.StorageStateUnresponsive {
		view.Reason = PreparationReasonStorageUnresponsive
		// The agent still reports the frozen copy: keep showing the bytes it
		// has written, without a rate.
		if p := knownCopyPhase(observation); p != nil {
			view.Phase, view.TotalKnown, view.IdleSeconds, view.Observed = p.Phase, p.TotalKnown, p.IdleSeconds, true
		}
		return view
	}
	view.RateShown = rowTransfer
	if file == nil || file.State == proto.MFAbsent {
		switch cachePhase {
		case prepobs.PhaseEvicting:
			return ModelPreparation{Phase: prepobs.PhaseEvicting}
		case prepobs.PhaseEvictionFailed:
			return ModelPreparation{Phase: prepobs.PhaseEvictionFailed, Reason: PreparationReasonEvictionFailed}
		}
	}
	if rowTransfer {
		switch file.State {
		case proto.MFDownloading:
			view.Phase, view.Reason = PreparationDownloading, ""
			if file.Acquisition == proto.ModelAcquirePeer {
				view.Phase = PreparationSyncing
			}
		case proto.MFSyncing:
			view.Phase, view.Reason = PreparationSyncing, ""
		}
		return view
	}
	p := knownCopyPhase(observation)
	if p == nil {
		return view
	}
	view.Phase = p.Phase
	view.Reason, view.TotalKnown, view.IdleSeconds, view.Observed = "", p.TotalKnown, p.IdleSeconds, true
	view.RateShown = p.Phase == proto.ModelPhaseCopying && !p.Stalled
	if p.Stalled {
		view.Reason = PreparationReasonCopyStalled
		return view
	}
	if p.Phase == proto.ModelPhaseCopying && p.TotalKnown && p.RateStable && observation.RateBytesPerS > 0 && observation.BytesTotal > observation.BytesDone {
		view.ETASeconds = (observation.BytesTotal - observation.BytesDone) / observation.RateBytesPerS
	}
	return view
}

// knownCopyPhase is the observation's preparation when it is in a phase this
// server knows; an older or newer agent's unknown phase shows as waiting.
func knownCopyPhase(observation *proto.ModelFileStatus) *proto.ModelPreparation {
	if observation == nil || observation.Preparation == nil {
		return nil
	}
	switch observation.Preparation.Phase {
	case proto.ModelPhaseChecking, proto.ModelPhaseCopying, proto.ModelPhaseVerifying, proto.ModelPhaseInstalling:
		return observation.Preparation
	}
	return nil
}
