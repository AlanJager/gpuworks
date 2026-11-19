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
// know the store or the API) to the hooks it feeds: the source of the
// plugins' feature keys, the node insert guard, the scheduling holder and
// the console banners. The adapters only convert types; each still answers
// with one atomic load and map lookups, without allocating (the banners
// convert once per snapshot). They read pushed state only through
// pushstate.Evaluator, whose every method the allocation test measures.
type PushedStateHooks struct{ state pushstate.Evaluator }

// NewPushedStateHooks wraps s.
func NewPushedStateHooks(s pushstate.Evaluator) PushedStateHooks { return PushedStateHooks{state: s} }

// Features is the source of the feature keys installed packages declare,
// the one the plugin host declares them with in the dynamic feature set.
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

type pushedHolder struct{ state pushstate.Evaluator }

func (pushedHolder) Source() store.SchedulingSource { return pushstate.HoldSource }

func (p pushedHolder) Hold(f store.NodeHoldFacts) store.NodeHold {
	return store.NodeHold(p.state.Hold(f.ID, f.OwnedCards))
}

// pushedBanners answers pushed state's banners for one viewer.
type pushedBanners struct {
	state        pushstate.Evaluator
	admin, other bannerCache
}

// adminRole is the role that sees administrator banners.
const adminRole = "admin"

func (b *pushedBanners) Banners(_ context.Context, viewer auth.Claims) []dto.ConsoleBanner {
	if viewer.Role == adminRole {
		return b.admin.convert(b.state.Banners(true))
	}
	return b.other.convert(b.state.Banners(false))
}

// bannerCache converts a banner list once and answers the converted list
// until the list changes. A list is never modified after it is published,
// so the address of its first banner identifies it; the cache keeps that
// list alive, so the address is not reused while it is cached.
type bannerCache struct {
	last atomic.Pointer[convertedBanners]
}

type convertedBanners struct {
	src *pushstate.Banner
	out []dto.ConsoleBanner
}

func (c *bannerCache) convert(list []pushstate.Banner) []dto.ConsoleBanner {
	if len(list) == 0 {
		return nil
	}
	if cur := c.last.Load(); cur != nil && cur.src == &list[0] {
		return cur.out
	}
	out := make([]dto.ConsoleBanner, 0, len(list))
	for _, banner := range list {
		out = append(out, dto.ConsoleBanner{ID: banner.ID, Severity: banner.Severity, Link: banner.Link,
			Message: dto.ProductMessage{Code: banner.Notice.Code(), Params: banner.Notice.Params(), Message: banner.Notice.Error()}})
	}
	c.last.Store(&convertedBanners{src: &list[0], out: out})
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
