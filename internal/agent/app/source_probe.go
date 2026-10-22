package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"gpuworks.ai/gpuworks/internal/agent/modelfetch"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// maxProbeBases bounds one source.probe: a rule's endpoints and its site.
const maxProbeBases = 17

// rpcSourceProbe asks each address whether it serves a repository, in
// parallel, through the client and with the login a download from it would
// use: the answer is about the path this node's downloads take.
func (n *NodeRuntime) rpcSourceProbe(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.SourceProbeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Source != modelrepo.SourceHF && p.Source != modelrepo.SourceMS {
		return nil, fmt.Errorf("source.probe: unsupported source %q", p.Source)
	}
	if p.Repo == "" || len(p.Bases) == 0 || len(p.Bases) > maxProbeBases {
		return nil, fmt.Errorf("source.probe needs a repository and 1 to %d addresses", maxProbeBases)
	}
	net := n.network()
	logins := n.modelLogins()
	out := proto.SourceProbeResult{Outcomes: make([]proto.SourceProbeOutcome, len(p.Bases))}
	var wg sync.WaitGroup
	for i, base := range p.Bases {
		wg.Add(1)
		go func(i int, base string) {
			defer wg.Done()
			client, err := modelSourceClient(p.Source, base, net)
			if err != nil {
				out.Outcomes[i] = proto.SourceProbeOutcome{Base: base, Status: proto.SourceProbeUnreachable, Error: err.Error()}
				return
			}
			out.Outcomes[i] = modelfetch.Probe(ctx, p.Source, p.Repo, base, client, logins)
		}(i, base)
	}
	wg.Wait()
	return out, nil
}
