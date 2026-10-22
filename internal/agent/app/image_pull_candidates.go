package app

import (
	"context"
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// pullImageCandidates serves an image.pull that carries the whole candidate
// list: the same walk an instance's image gets (ranking, logins, stall watch,
// cooldown), with the first source that works tagged back to the canonical
// reference. It reports through image.node status like the single-reference
// pull, and names the source that worked in Source.
func (n *NodeRuntime) pullImageCandidates(ctx context.Context, d runtime.Driver, p proto.ImagePullParams) {
	status := func(state, source, message string) {
		n.reportImageNode(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: state, TransferID: p.TransferID, Source: source, Message: message})
	}
	// One pulling state is overwritten by its successor on the wire, so what
	// the walk learns on the way (the probed order, a source that failed) is
	// also kept for the final message.
	var notes []string
	res, err := n.puller.Walk(ctx, d, p.Candidates, imagepull.Options{NodeProxy: true}, imagepull.Hooks{
		Ordered: func(summary string) {
			notes = append(notes, "image sources by probe: "+summary)
			status("pulling", "", "image sources by probe: "+summary)
		},
		Attempt: func(ref string) { status("pulling", ref, "pulling "+ref) },
		// Every line is offered to the egress gate, which keeps at most one
		// per second for this image and always delivers the last one.
		Line: func(ref, line string, _ imagepull.Snapshot, _ bool) { status("pulling", ref, line) },
		Failed: func(ref string, err error) {
			n.log.Warn("image pull source failed", "image", p.CanonicalRef, "ref", ref, "err", err)
			notes = append(notes, "image source failed: "+ref+": "+err.Error())
			status("pulling", ref, "image source failed: "+ref+": "+err.Error())
		},
		Pulled: func(ctx context.Context, ref string) error {
			if ref == p.CanonicalRef {
				return nil
			}
			tagger, ok := d.(interface {
				TagImage(context.Context, string, string) error
			})
			if !ok {
				return errors.New("runtime does not support canonical image tagging")
			}
			if err := tagger.TagImage(ctx, ref, p.CanonicalRef); err != nil {
				return errors.New("tag canonical image: " + err.Error())
			}
			return nil
		},
	})
	if err != nil {
		var allFailed *imagepull.AllFailedError
		if errors.As(err, &allFailed) && ctx.Err() == nil {
			status("failed", "", err.Error())
		} else {
			status("failed", "", imageOperationError(ctx, err))
		}
		return
	}
	message := "image pulled and tagged"
	if len(notes) > 0 {
		message += "; " + strings.Join(notes, "; ")
	}
	done := proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "present", TransferID: p.TransferID, Source: res.Ref, Message: message}
	if inspector, ok := d.(runtime.ImageInspector); ok {
		if inspected, inspectErr := inspector.InspectImage(ctx, p.CanonicalRef); inspectErr == nil {
			done.ImageID = inspected.ID
		}
	}
	n.reportImageNode(done)
}

// validPullCandidates trims the candidate list an image.pull carries and
// refuses a digest reference: the image is tagged under its canonical name,
// which a runtime cannot do for one.
func validPullCandidates(in []proto.ImagePullCandidate) ([]proto.ImagePullCandidate, error) {
	out := make([]proto.ImagePullCandidate, 0, len(in))
	for _, c := range in {
		c.Ref = strings.TrimSpace(c.Ref)
		if c.Ref == "" {
			continue
		}
		if strings.Contains(c.Ref, "@sha256:") {
			return nil, errors.New("digest image refs are not supported")
		}
		out = append(out, c)
	}
	return out, nil
}
