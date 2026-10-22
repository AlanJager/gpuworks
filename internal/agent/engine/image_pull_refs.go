package engine

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/proto"
)

// WithImagePuller injects the node's puller. The node shares one with its
// image.pull RPC, so what one pull learns about a registry (it just failed)
// steers the next.
func WithImagePuller(puller *imagepull.Puller) EngineOption {
	return func(engine *Engine) {
		engine.puller = puller
	}
}

// pullCandidates is where this node pulls spec's image from, in order. A
// current server plans the whole list (spec.ImagePullCandidates, the node's
// pull source rules already applied), and it is used exactly as sent: the node
// has no say in what the list holds, only in which of the candidates marked
// for ranking it tries first. A desired state stored by an older server has
// none; its flat list is tried as it is. serverPlanned tells the two apart.
func (e *Engine) pullCandidates(spec proto.InstanceDesiredState) (candidates []proto.ImagePullCandidate, serverPlanned bool) {
	if len(spec.ImagePullCandidates) > 0 {
		return spec.ImagePullCandidates, true
	}
	refs := imagePullCandidates(spec)
	candidates = make([]proto.ImagePullCandidate, len(refs))
	for i, ref := range refs {
		candidates[i] = proto.ImagePullCandidate{Ref: ref}
	}
	return candidates, false
}

func candidateRefs(candidates []proto.ImagePullCandidate) []string {
	refs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if ref := strings.TrimSpace(c.Ref); ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

// pullOptions says how to pull spec's image: through the node's proxy for a
// server-planned list, through the deployment's own proxy choice for the old
// shape that carried one.
func (e *Engine) pullOptions(spec proto.InstanceDesiredState, serverPlanned bool) imagepull.Options {
	opts := imagepull.Options{LaunchIntent: true, NodeProxy: serverPlanned}
	if !serverPlanned && (spec.ImagePullExplicit || spec.ImagePullProxy != "") {
		proxy := spec.ImagePullProxy
		opts.LegacyProxy = &proxy
	}
	return opts
}

// imagePullCandidates is the ordered list of registry references an older
// server's desired state names for an instance's image: the flat pull order,
// else the single pull ref, else the image itself. Blank and repeated
// references are dropped. A network URL rewrite never applied here: it names
// HTTP addresses, never registry references.
func imagePullCandidates(spec proto.InstanceDesiredState) []string {
	ordered := make([]string, 0, len(spec.ImagePullRefs)+1)
	for _, ref := range spec.ImagePullRefs {
		if ref = strings.TrimSpace(ref); ref != "" {
			ordered = append(ordered, ref)
		}
	}
	if len(ordered) == 0 {
		if ref := strings.TrimSpace(spec.ImagePullRef); ref != "" {
			ordered = append(ordered, ref)
		} else if ref := strings.TrimSpace(spec.Image); ref != "" {
			ordered = append(ordered, ref)
		}
	}
	out := make([]string, 0, len(ordered))
	seen := make(map[string]struct{}, len(ordered))
	for _, ref := range ordered {
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	return out
}
