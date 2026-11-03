package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func (n *NodeRuntime) rpcImagePull(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.ImagePullParams
	if err := json.Unmarshal(raw, &p); err != nil || strings.TrimSpace(p.EffectiveRef) == "" || strings.TrimSpace(p.CanonicalRef) == "" {
		return nil, errors.New("image pull requires effective_ref and canonical_ref")
	}
	if strings.Contains(p.EffectiveRef, "@sha256:") || strings.Contains(p.CanonicalRef, "@sha256:") {
		return nil, errors.New("digest image refs are not supported")
	}
	p.EffectiveRef = strings.TrimSpace(p.EffectiveRef)
	p.CanonicalRef = strings.TrimSpace(p.CanonicalRef)
	candidates, err := validPullCandidates(p.Candidates)
	if err != nil {
		return nil, err
	}
	p.Candidates = candidates
	go func() {
		pullCtx, cancelPull := context.WithTimeout(context.WithoutCancel(ctx), imageOperationTimeout)
		defer cancelPull()
		unregister := n.beginImageOperation(p.CanonicalRef, p.TransferID, cancelPull)
		defer unregister()
		n.lockImageOperation(p.CanonicalRef, "pulling")
		defer n.imagePullMu.Unlock()
		n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "pulling", TransferID: p.TransferID, Message: "pulling " + p.EffectiveRef})
		if !n.host.PullsImages() {
			// Every image is on a simulated node; asking a registry would
			// reach the network from the real host.
			n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "present", TransferID: p.TransferID, Message: "image present"})
			return
		}
		d := n.runtimeDriver()
		if d == nil {
			n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "failed", TransferID: p.TransferID, Message: "runtime driver is unavailable"})
			return
		}
		if len(p.Candidates) > 0 {
			n.pullImageCandidates(pullCtx, d, p)
			return
		}
		// Every pull line is offered to the egress gate, which keeps at most
		// one per second for this image and always delivers the last one.
		progress := imagepull.NewProgress()
		if err := d.PullImage(pullCtx, p.EffectiveRef, func(line string) {
			_, done, total, rate, _, _ := progress.Observe(line, time.Now())
			n.reportImageNode(proto.ImageNodeStatus{
				ImageRef: p.CanonicalRef, State: "pulling", TransferID: p.TransferID, Message: line,
				TransferredBytes: done, SizeBytes: total, RateBPS: rate,
			})
		}); err != nil {
			n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "failed", TransferID: p.TransferID, Message: imageOperationError(pullCtx, err)})
			return
		}
		tagger, ok := d.(interface {
			TagImage(context.Context, string, string) error
		})
		if !ok {
			n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "failed", TransferID: p.TransferID, Message: "runtime does not support canonical image tagging"})
			return
		}
		if err := tagger.TagImage(pullCtx, p.EffectiveRef, p.CanonicalRef); err != nil {
			n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "failed", TransferID: p.TransferID, Message: "tag canonical image: " + err.Error()})
			return
		}
		status := proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "present", TransferID: p.TransferID, Message: "image pulled and tagged"}
		if inspector, ok := d.(runtime.ImageInspector); ok {
			if inspected, inspectErr := inspector.InspectImage(pullCtx, p.CanonicalRef); inspectErr == nil {
				status.ImageID = inspected.ID
			}
		}
		n.reportImageNode(status)
	}()
	return proto.ImagePullResult{State: "pulling", EffectiveRef: p.EffectiveRef, CanonicalRef: p.CanonicalRef}, nil
}
