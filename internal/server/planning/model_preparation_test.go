package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPreparationDistinguishesStallOfflineAndStorageHealth(t *testing.T) {
	node := &store.Node{State: store.NodeStateOnline}
	source := store.ModelSource{Location: store.ModelLocation{File: &store.ModelFile{State: proto.MFDownloading}}}
	observation := &proto.ModelFileStatus{BytesTotal: 100, BytesDone: 40, RateBytesPerS: 10, Preparation: &proto.ModelPreparation{Attempt: "a", Phase: proto.ModelPhaseCopying, TotalKnown: true, RateStable: true}}
	if got := PrepareModelView(node, source, observation, ""); got.ETASeconds != 6 || got.Reason != "" {
		t.Fatal(got)
	}
	observation.Preparation.Stalled = true
	if got := PrepareModelView(node, source, observation, ""); got.Reason != "MODEL_COPY_STALLED" || got.ETASeconds != 0 {
		t.Fatal(got)
	}
	source.Location.NodeState = &store.StorageNodeState{State: proto.StorageStateUnresponsive}
	// Storage health outranks the copy's own stall, and the bytes the frozen
	// copy wrote stay visible without a rate.
	if got := PrepareModelView(node, source, observation, ""); got.Reason != "MODEL_STORAGE_UNRESPONSIVE" || !got.Observed || !got.TotalKnown || got.RateShown || got.Phase != proto.ModelPhaseCopying {
		t.Fatal(got)
	}
	if got := PrepareModelView(node, source, nil, ""); got.Reason != "MODEL_STORAGE_UNRESPONSIVE" || got.Observed || got.Phase != PreparationWaiting {
		t.Fatal(got)
	}
	node.State = store.NodeStateOffline
	if got := PrepareModelView(node, source, observation, ""); got.Reason != "MODEL_NODE_OFFLINE" {
		t.Fatal(got)
	}
	node.State = store.NodeStateOnline
	source.Location.NodeState = nil
	if got := PrepareModelView(node, source, nil, ""); got.Reason != "MODEL_PREPARATION_WAITING" {
		t.Fatal(got)
	}
	observation.Preparation.Phase = "future"
	if got := PrepareModelView(node, source, observation, ""); got.Phase != "waiting" {
		t.Fatal(got)
	}
	source.Location.Ready = true
	if got := PrepareModelView(node, source, observation, ""); got.Phase != "ready" {
		t.Fatal(got)
	}
}

func TestPreparationShowsEvictionWhenResumingWithdrawnCache(t *testing.T) {
	node := &store.Node{State: store.NodeStateOnline}
	for _, file := range []*store.ModelFile{nil, {State: proto.MFAbsent}} {
		source := store.ModelSource{Location: store.ModelLocation{File: file}}
		if got := PrepareModelView(node, source, nil, prepobs.PhaseEvicting); got.Phase != "evicting" || got.Reason != "" {
			t.Fatal(got)
		}
		if got := PrepareModelView(node, source, nil, prepobs.PhaseEvictionFailed); got.Phase != "eviction_failed" || got.Reason != "MODEL_CACHE_EVICTION_FAILED" {
			t.Fatal(got)
		}
	}
}

func TestPreparationDecidesRetryAndWhatTransferIsShown(t *testing.T) {
	node := &store.Node{State: store.NodeStateOnline}
	cache := store.Storage{Type: proto.StorageNFSCache}
	failed := &store.ModelFile{State: proto.MFFailed, Acquisition: proto.ModelAcquireCopy, TransferID: "intent"}
	got := PrepareModelView(node, store.ModelSource{Location: store.ModelLocation{Storage: cache, File: failed}}, nil, "")
	if !got.Retryable || got.Reason != PreparationReasonFailed || got.RateShown {
		t.Fatalf("failed cache copy: %+v", got)
	}
	for _, file := range []*store.ModelFile{
		{State: proto.MFFailed, Acquisition: proto.ModelAcquireCopy},                      // legacy intent without an ID
		{State: proto.MFFailed, Acquisition: proto.ModelAcquireDownload, TransferID: "x"}, // not a copy
		{State: proto.MFDownloading, Acquisition: proto.ModelAcquireCopy, TransferID: "x"},
	} {
		if PrepareModelView(node, store.ModelSource{Location: store.ModelLocation{Storage: cache, File: file}}, nil, "").Retryable {
			t.Fatalf("retry offered for %+v", file)
		}
	}
	if PrepareModelView(node, store.ModelSource{Location: store.ModelLocation{Storage: store.Storage{Type: proto.StorageLocal}, File: failed}}, nil, "").Retryable {
		t.Fatal("retry offered outside a cache")
	}
	download := store.ModelSource{Location: store.ModelLocation{File: &store.ModelFile{State: proto.MFDownloading, Acquisition: proto.ModelAcquireDownload, SizeBytes: 100}}}
	if got := PrepareModelView(node, download, nil, ""); !got.TotalKnown || !got.RateShown || got.Observed {
		t.Fatalf("download: %+v", got)
	}
	node.State = store.NodeStateOffline
	if got := PrepareModelView(node, download, nil, ""); got.TotalKnown || got.RateShown {
		t.Fatalf("offline download still shows transfer: %+v", got)
	}
	node.State = store.NodeStateOnline
	copying := store.ModelSource{Location: store.ModelLocation{Storage: cache, File: &store.ModelFile{State: proto.MFDownloading, Acquisition: proto.ModelAcquireCopy}}}
	observation := &proto.ModelFileStatus{BytesTotal: 100, BytesDone: 50, RateBytesPerS: 5, Preparation: &proto.ModelPreparation{Phase: proto.ModelPhaseVerifying, TotalKnown: true}}
	if got := PrepareModelView(node, copying, observation, ""); !got.Observed || got.RateShown {
		t.Fatalf("verifying shows a copy rate: %+v", got)
	}
	observation.Preparation.Phase = proto.ModelPhaseCopying
	if got := PrepareModelView(node, copying, observation, ""); !got.Observed || !got.RateShown {
		t.Fatalf("copying: %+v", got)
	}
}

func TestPreparationUsesSharedTransferWhileWaitingForStorage(t *testing.T) {
	node := &store.Node{State: store.NodeStateOnline}
	for _, kind := range []string{proto.StorageNFS, proto.StorageSharedDir, proto.StorageNFSCache} {
		t.Run(kind, func(t *testing.T) {
			shared := &store.ModelFile{ID: 1, State: proto.MFDownloading, Acquisition: proto.ModelAcquireDownload, SizeBytes: 100, Downloaded: 40}
			source := store.ModelSource{Action: store.SourceWaitShared, Location: store.ModelLocation{Storage: store.Storage{Type: kind}, Shared: shared, Reason: store.LocationModelMissingInStore}}
			if kind == proto.StorageNFSCache {
				source.Location.File = &store.ModelFile{ID: 2, State: proto.MFFailed, Acquisition: proto.ModelAcquireCopy, TransferID: "old"}
			}
			got := PrepareModelView(node, source, nil, "")
			if got.Phase != "downloading" || got.Reason != "" || !got.TotalKnown || !got.RateShown || got.Retryable || got.Observed {
				t.Fatalf("shared download: %+v", got)
			}
			shared.Acquisition, shared.State = proto.ModelAcquirePeer, proto.MFSyncing
			if got := PrepareModelView(node, source, nil, ""); got.Phase != "syncing" || got.Reason != "" {
				t.Fatalf("shared sync: %+v", got)
			}
			shared.SizeBytes = 0
			if got := PrepareModelView(node, source, nil, ""); got.TotalKnown {
				t.Fatalf("unknown shared total: %+v", got)
			}
			shared.SizeBytes, shared.State = 100, proto.MFFailed
			source.Action = store.SourceRefused
			if got := PrepareModelView(node, source, nil, ""); got.Reason != PreparationReasonFailed || got.Retryable || got.RateShown {
				t.Fatalf("failed shared download: %+v", got)
			}
			shared.State = proto.MFDownloading
			source.Action = store.SourceWaitShared
			source.Location.NodeState = &store.StorageNodeState{State: proto.StorageStateUnresponsive}
			if got := PrepareModelView(node, source, nil, ""); got.Reason != PreparationReasonStorageUnresponsive || got.RateShown {
				t.Fatalf("unresponsive storage: %+v", got)
			}
			node.State = store.NodeStateOffline
			if got := PrepareModelView(node, source, nil, ""); got.Reason != PreparationReasonNodeOffline || got.RateShown {
				t.Fatalf("offline node: %+v", got)
			}
			node.State = store.NodeStateOnline
		})
	}
}

func TestPreparationCacheCopyUsesLocalRowAfterSharedDownload(t *testing.T) {
	local := &store.ModelFile{ID: 2, State: proto.MFDownloading, Acquisition: proto.ModelAcquireCopy, TransferID: "cache"}
	source := store.ModelSource{Action: store.SourceFillCache, Location: store.ModelLocation{
		Storage: store.Storage{Type: proto.StorageNFSCache}, File: local,
		Shared: &store.ModelFile{ID: 1, State: proto.MFPresent, Acquisition: proto.ModelAcquireDownload, SizeBytes: 100}, Reason: store.LocationNotCached,
	}}
	observation := &proto.ModelFileStatus{BytesTotal: 100, BytesDone: 20, Preparation: &proto.ModelPreparation{Phase: proto.ModelPhaseCopying, TotalKnown: true}}
	if got := PrepareModelView(&store.Node{State: store.NodeStateOnline}, source, observation, ""); got.Phase != proto.ModelPhaseCopying || !got.Observed || !got.TotalKnown {
		t.Fatalf("local cache copy: %+v", got)
	}
}
