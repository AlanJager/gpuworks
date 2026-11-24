package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// nodeImages is the node's image work: which catalog images it is asked to
// inventory, the single slot image operations take, the operations that can
// be cancelled, the peer transfers in flight, and the image RPCs. It owns
// that state and its locks (ADR 0066).
type nodeImages struct {
	// wired guards wire, so a NodeRuntime built as a literal is as usable as
	// a constructed one (NodeRuntime.imageWork).
	wired sync.Once
	deps  imageDeps

	// slot serializes image operations: one pull, load or copy at a time.
	slot sync.Mutex
	// ops is the running operations a cancel can name.
	ops imageOperations

	// refs is the catalog images the server asked this node to inventory.
	refsMu sync.RWMutex
	refs   []string
	// seen is what the inventory last concluded per image, so only a change
	// is logged (image_presence_log.go).
	seen imagePresenceLog

	// syncs is the peer transfers this node is receiving, by image
	// reference; sends is the ones it is sending, by transfer id. syncMu
	// guards both and every activeImageSync.
	syncMu sync.Mutex
	syncs  map[string]*activeImageSync
	sends  map[string]context.CancelFunc
}

// imageDeps is what image work needs from the node; image code holds none of
// the node's locks. Each is called when needed, not kept: the server can
// replace the runtime, the network configuration and the models root while
// the agent runs. The logger, the puller and the state directory are
// functions because tests replace them on a built node.
type imageDeps struct {
	log    func() *slog.Logger
	driver func() runtime.Driver
	// report sends one image status to the server.
	report func(proto.ImageNodeStatus)
	// pullsImages is false on a simulated node, which has every image.
	pullsImages func() bool
	puller      func() *imagepull.Puller
	network     func() proto.NetworkConfig
	modelsRoot  func() string
	stateDir    func() string
	// phaseReporter reports a peer transfer's phases when asked to.
	phaseReporter func(enabled bool, id, transport string) func(string)
}

// imageWork is the node's image work, wired to the node on first use.
func (n *NodeRuntime) imageWork() *nodeImages {
	n.images.wired.Do(func() {
		n.images.wire(imageDeps{
			log:           func() *slog.Logger { return n.log },
			driver:        n.runtimeDriver,
			report:        n.reportImageNode,
			pullsImages:   func() bool { return n.host.PullsImages() },
			puller:        func() *imagepull.Puller { return n.puller },
			network:       n.network,
			modelsRoot:    n.ModelsRoot,
			stateDir:      func() string { return n.stateDir },
			phaseReporter: n.peerTransferPhaseReporter,
		})
	})
	return &n.images
}

// wire gives the image work its dependencies and empty transfer tables.
func (i *nodeImages) wire(deps imageDeps) {
	i.deps = deps
	i.syncs = map[string]*activeImageSync{}
	i.sends = map[string]context.CancelFunc{}
}

// driverName is the runtime's name, "" when the node has none.
func (i *nodeImages) driverName() string {
	if d := i.deps.driver(); d != nil {
		return d.Name()
	}
	return ""
}

// imageHandlers is the image RPCs by method name.
var imageHandlers = map[string]func(*nodeImages, context.Context, json.RawMessage) (any, error){
	proto.RPCImagePull:        (*nodeImages).rpcImagePull,
	proto.RPCImageCancel:      (*nodeImages).rpcImageCancel,
	proto.RPCImageRemove:      (*nodeImages).rpcImageRemove,
	proto.RPCImageInspect:     (*nodeImages).rpcImageInspect,
	proto.RPCImageExists:      (*nodeImages).rpcImageExists,
	proto.RPCImageLoadURL:     (*nodeImages).rpcImageLoadURL,
	proto.RPCImageLoadPath:    (*nodeImages).rpcImageLoadPath,
	proto.RPCImagePreviewPath: (*nodeImages).rpcImagePreviewPath,
	proto.RPCImageSyncListen:  (*nodeImages).rpcImageSyncListen,
	proto.RPCImageSyncSend:    (*nodeImages).rpcImageSyncSend,
	proto.RPCImageSyncProbe:   (*nodeImages).rpcImageSyncProbe,
	proto.RPCImageSyncStop:    (*nodeImages).rpcImageSyncStop,
}

// handler is the handler of an image RPC, nil when method is not one.
func (i *nodeImages) handler(method string) func(context.Context, json.RawMessage) (any, error) {
	if handle := runtimeImageHandlers[method]; handle != nil {
		return func(ctx context.Context, raw json.RawMessage) (any, error) { return handle(i, ctx, method, raw) }
	}
	if handle := imageHandlers[method]; handle != nil {
		return func(ctx context.Context, raw json.RawMessage) (any, error) { return handle(i, ctx, raw) }
	}
	return nil
}

// lockSlot takes the node's image slot, and reports that the operation is
// waiting when another one already holds it.
//
// A queued transfer that says nothing is indistinguishable from a stalled one:
// both show up as a progress bar that never moves. Saying which one it is is
// the difference between "wait, the node is busy" and "this is broken".
func (i *nodeImages) lockSlot(imageRef, state string) {
	if i.slot.TryLock() {
		return
	}
	i.deps.report(proto.ImageNodeStatus{
		ImageRef: imageRef, State: state,
		Message: "waiting for another image operation on this node to finish",
	})
	i.slot.Lock()
}

// replaceCatalog sets the images to inventory, from a full state sync.
func (i *nodeImages) replaceCatalog(refs []string) {
	i.refsMu.Lock()
	i.refs = append([]string(nil), refs...)
	i.refsMu.Unlock()
	i.seen.forget(refs)
}

// updateCatalog sets the images to inventory when the set of them differs
// from the one held, and reports whether it did.
func (i *nodeImages) updateCatalog(refs []string) bool {
	i.refsMu.Lock()
	changed := !sameImageRefSet(i.refs, refs)
	if changed {
		i.refs = append([]string(nil), refs...)
	}
	i.refsMu.Unlock()
	i.seen.forget(refs)
	return changed
}

func sameImageRefSet(left, right []string) bool {
	leftSet := make(map[string]struct{}, len(left))
	for _, ref := range left {
		leftSet[ref] = struct{}{}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, ref := range right {
		rightSet[ref] = struct{}{}
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for ref := range leftSet {
		if _, ok := rightSet[ref]; !ok {
			return false
		}
	}
	return true
}

// catalog is a copy of the images to inventory.
func (i *nodeImages) catalog() []string {
	i.refsMu.RLock()
	defer i.refsMu.RUnlock()
	return append([]string(nil), i.refs...)
}

// presence asks the runtime about each of refs, the whole catalog when refs
// is empty. An image the runtime could not answer for is left out, so the
// server keeps what it last knew.
func (i *nodeImages) presence(ctx context.Context, refs []string) []proto.ImagePresence {
	d := i.deps.driver()
	if d == nil {
		return nil
	}
	if len(refs) == 0 {
		refs = i.catalog()
	}
	log := i.deps.log()
	var images []proto.ImagePresence
	for _, ref := range refs {
		var present bool
		var imageID string
		var size *int64
		var err error
		if reporter, ok := d.(runtime.ImagePresenceReporter); ok {
			present, imageID, size, err = reporter.ImagePresence(ctx, ref)
		} else {
			present, err = d.ImageExists(ctx, ref)
		}
		if err != nil {
			log.Warn("image presence inspection failed", "image", ref, "runtime", d.Name(), "err", err)
			continue
		}
		i.seen.observe(ctx, log, d, ref, present)
		images = append(images, proto.ImagePresence{ImageRef: ref, Present: present, ImageID: imageID, RuntimeSizeBytes: size})
	}
	return images
}
