package app

import (
	"context"
	"sync/atomic"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PushedStateHooks adapts pushed state (package pushstate, which does not
// know the store or the API) to the hooks it feeds: the feature gate's
// source, the node insert guard, the scheduling holder and the console
// banners. The adapters only convert types; each still answers with one
// atomic load and map lookups, without allocating (the banners convert once
// per snapshot).
type PushedStateHooks struct{ state *pushstate.State }

// NewPushedStateHooks wraps s.
func NewPushedStateHooks(s *pushstate.State) PushedStateHooks { return PushedStateHooks{state: s} }

// Features is the feature gate's source (Host.ProvideFeatures).
func (h PushedStateHooks) Features() featuregate.Source { return h.state }

// GuardNode is the node insert guard (Host.GuardNodeInsert).
func (h PushedStateHooks) GuardNode(c store.FleetCapacity) *messages.ProductError {
	return h.state.GuardNode(c.NodesWithCards, c.OwnedCards)
}

// Holder is the scheduling holder (Host.HoldScheduling), under the one
// source pushstate.HoldSource.
func (h PushedStateHooks) Holder() store.SchedulingHolder { return pushedHolder(h) }

// Banners is the console banner source (Host.AddBannerSource).
func (h PushedStateHooks) Banners() api.BannerSource { return &pushedBanners{state: h.state} }

type pushedHolder struct{ state *pushstate.State }

func (pushedHolder) Source() store.SchedulingSource { return pushstate.HoldSource }

func (p pushedHolder) Hold(f store.NodeHoldFacts) store.NodeHold {
	return store.NodeHold(p.state.Hold(f.ID, f.OwnedCards))
}

// pushedBanners converts a snapshot's banners once per snapshot and
// audience and answers the converted list until the snapshot changes. A
// snapshot is never modified after it is published, so the address of its
// first banner identifies the list; the cache keeps that list alive, so the
// address is not reused while it is cached.
type pushedBanners struct {
	state        *pushstate.State
	admin, other atomic.Pointer[convertedBanners]
}

type convertedBanners struct {
	src *pushstate.Banner
	out []dto.ConsoleBanner
}

// adminRole is the role that sees administrator banners.
const adminRole = "admin"

func (b *pushedBanners) Banners(_ context.Context, viewer auth.Claims) []dto.ConsoleBanner {
	admin := viewer.Role == adminRole
	list := b.state.Banners(admin)
	if len(list) == 0 {
		return nil
	}
	cache := &b.other
	if admin {
		cache = &b.admin
	}
	if c := cache.Load(); c != nil && c.src == &list[0] {
		return c.out
	}
	out := make([]dto.ConsoleBanner, 0, len(list))
	for _, banner := range list {
		out = append(out, dto.ConsoleBanner{ID: banner.ID, Severity: banner.Severity, Link: banner.Link,
			Message: dto.ProductMessage{Code: banner.Notice.Code(), Params: banner.Notice.Params(), Message: banner.Notice.Error()}})
	}
	cache.Store(&convertedBanners{src: &list[0], out: out})
	return out
}

// PushedStateFleet reads the fleet for pushed state's capacity selection
// (store.NodeFleet, one query on the read pool), ordered by id.
func PushedStateFleet(st *store.Store) pushstate.FleetReader {
	return func(ctx context.Context) ([]fleetcap.Node, error) {
		facts, err := st.NodeFleet(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]fleetcap.Node, len(facts))
		for i, f := range facts {
			out[i] = fleetcap.Node{ID: f.ID, Cards: int64(f.OwnedCards)}
		}
		return out, nil
	}
}
