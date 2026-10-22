package hub

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// imagePullCandidate is one planned reference with the source label the image
// ledger records for it.
type imagePullCandidate struct {
	proto.ImagePullCandidate
	Source string
}

// pullCandidates plans where node pulls a catalog image from: the copies are
// the catalog's (or the caller's, a deployment's template sources included),
// planned against the node's pull source rules (planning.PullCandidatesFor),
// the same plan a launch on that node makes.
func (h *Hub) pullCandidates(image *store.Image, node *store.Node, copies []string) ([]imagePullCandidate, error) {
	if len(copies) == 0 {
		copies = h.store.ImageCopies(image.ImageRef, nil)
	}
	planned, err := planning.PullCandidatesFor(h.store, h.nodeResolver(), node, image.ImageRef, copies)
	if err != nil {
		return nil, err
	}
	out := make([]imagePullCandidate, 0, len(planned))
	for _, candidate := range planned {
		out = append(out, imagePullCandidate{ImagePullCandidate: candidate, Source: pullref.Source(candidate.Ref, image.ImageRef)})
	}
	return out, nil
}

// nodeResolver is the network resolution the hub's network service offers,
// for pull source rules and sync groups that select nodes by network.
func (h *Hub) nodeResolver() planning.NodeResolver {
	if resolver, ok := h.networks.(interface {
		Resolve([]store.Node) (netplan.Resolution, error)
	}); ok {
		return resolver.Resolve
	}
	return nil
}

// planReplay plans what a full replay sends node from the pull settings as
// they are now, selected once for the node: where each instance's image and
// each model download comes from, and the node's proxy. Settings that cannot
// be read stop the replay: an absent proxy would read as "go direct".
func (h *Hub) planReplay(node *store.Node, ds *proto.DesiredState) error {
	selection, err := planning.PullSettingsFor(h.store, h.nodeResolver(), node)
	if err != nil {
		return fmt.Errorf("read the pull settings for node %s: %w", node.UUID, err)
	}
	ds.Network = selection.Network()
	h.planReplayImages(node, planning.ImageRules(selection.Rules), ds.Instances)
	planned, refused := planning.PlanModelFileTasks(planning.URLRules(selection.Rules), node, ds.ModelFiles)
	for _, err := range refused {
		h.log.Warn("no allowed download source for model file", "node", node.UUID, "err", err)
	}
	ds.ModelFiles = planned
	return nil
}

// planReplayImages sets each instance's pull candidates for node from the
// node's image rules, so a rule changed since the instance was launched
// governs its next pull. An instance written before candidates existed has no
// copies recorded; its old pull order and the catalog's copies stand in. The
// deployment-scoped source and proxy older servers recorded are dropped:
// where a node pulls from is no deployment's choice any more.
func (h *Hub) planReplayImages(node *store.Node, imageRules []pullref.SourceRule, instances []proto.InstanceDesiredState) {
	for i := range instances {
		spec := &instances[i]
		if spec.DesiredState == "stopped" || strings.TrimSpace(spec.Image) == "" {
			continue
		}
		if len(spec.ImageSources) == 0 {
			spec.ImageSources = h.store.ImageCopies(spec.Image, spec.ImagePullRefs)
		}
		candidates, err := planning.PullCandidatesWith(imageRules, node, spec.Image, spec.ImageSources)
		if err != nil {
			h.log.Warn("no allowed pull source for instance image", "instance", spec.UUID, "node", node.UUID, "image", spec.Image, "err", err)
			continue
		}
		spec.ImagePullCandidates = candidates
		spec.ImagePullRefs = make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			spec.ImagePullRefs = append(spec.ImagePullRefs, candidate.Ref)
		}
		if len(spec.ImagePullRefs) > 0 {
			spec.ImagePullRef = spec.ImagePullRefs[0]
		}
		spec.ImagePullExplicit = false
		spec.ImagePullProxy = ""
	}
}

// watchImagePullCandidates advances the pull chain for an agent that walks
// only one reference per call (it predates proto.CapImagePullCandidates),
// after the agent reports an asynchronous terminal failure. The initial HTTP
// request remains quick, while image_nodes.message retains one audit entry per
// attempted ref.
func (h *Hub) watchImagePullCandidates(imageID, nodeID uint, nodeUUID string, rowID uint, canonical string, candidates []imagePullCandidate, current int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	failures := make([]string, 0, len(candidates))
	for {
		row, err := h.store.ImageNode(imageID, nodeID)
		if err != nil {
			return
		}
		if row.State == store.ImageStatePresent {
			return
		}
		if row.State == store.ImageStateFailed {
			failure := strings.TrimSpace(row.Message)
			if failure == "" {
				failure = "agent reported pull failure"
			}
			failures = append(failures, row.Source+": "+failure)
			current++
			if current >= len(candidates) {
				_, _ = h.store.UpsertImageNode(&store.ImageNode{ID: rowID, ImageVersionID: imageID, ImageID: imageID, NodeID: nodeID, State: store.ImageStateFailed, Source: row.Source, Message: boundedPullMessage("all image sources failed: " + strings.Join(failures, " | "))})
				return
			}
			next := candidates[current]
			_, _ = h.store.UpsertImageNode(&store.ImageNode{ID: rowID, ImageVersionID: imageID, ImageID: imageID, NodeID: nodeID, State: store.ImageStatePulling, Source: next.Source, Message: boundedPullMessage(strings.Join(failures, " | ") + " | trying " + next.Source + " " + next.Ref)})
			if _, callErr := h.CallNode(ctx, nodeUUID, proto.RPCImagePull, proto.ImagePullParams{EffectiveRef: next.Ref, CanonicalRef: canonical}); callErr != nil {
				failures = append(failures, next.Source+": "+callErr.Error())
				current++
				if current >= len(candidates) {
					_, _ = h.store.UpsertImageNode(&store.ImageNode{ID: rowID, ImageVersionID: imageID, ImageID: imageID, NodeID: nodeID, State: store.ImageStateFailed, Source: next.Source, Message: boundedPullMessage("all image sources failed: " + strings.Join(failures, " | "))})
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			message := "image pull source fallback timed out"
			if len(failures) > 0 {
				message = "all image sources failed or timed out: " + strings.Join(failures, " | ")
			}
			_, _ = h.store.UpsertImageNode(&store.ImageNode{ID: rowID, ImageVersionID: imageID, ImageID: imageID, NodeID: nodeID, State: store.ImageStateFailed, Source: row.Source, Message: boundedPullMessage(message)})
			return
		case <-ticker.C:
		}
	}
}

func boundedPullMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= 500 {
		return message
	}
	return message[:497] + "..."
}
