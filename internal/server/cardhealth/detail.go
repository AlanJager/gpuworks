package cardhealth

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Asking an agent for a check's last run. It is the one read that reaches an
// agent, so it is bounded: one request in flight per card and check however
// many people ask, each answer reused for detailCacheFor, at most
// detailPerNode requests in flight per node and detailInFlight in all. The
// agent answers from memory and runs nothing.

const (
	detailCacheFor = 5 * time.Second
	detailPerNode  = 2
	detailInFlight = 16
	detailTimeout  = 5 * time.Second
)

// ErrBusy is returned when too many detail requests are in flight.
var ErrBusy = errors.New("too many card check detail requests in flight")

// CallNode calls an RPC on a node's agent.
type CallNode func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error)

type detailKey struct{ node, ident, check string }

type detailCall struct {
	done   chan struct{}
	detail proto.CardCheckDetail
	err    error
	at     time.Time
}

// Details fetches check details from agents.
type Details struct {
	call CallNode
	now  func() time.Time

	mu      sync.Mutex
	calls   map[detailKey]*detailCall
	perNode map[string]int
	total   int
}

// NewDetails returns a fetcher calling agents through call.
func NewDetails(call CallNode) *Details {
	return &Details{call: call, now: time.Now, calls: map[detailKey]*detailCall{}, perNode: map[string]int{}}
}

// Detail returns the last run of one check on one card.
func (d *Details) Detail(ctx context.Context, nodeUUID, ident, check string) (proto.CardCheckDetail, error) {
	key := detailKey{nodeUUID, ident, check}
	d.mu.Lock()
	if c, ok := d.calls[key]; ok {
		select {
		case <-c.done:
			if d.now().Sub(c.at) < detailCacheFor {
				d.mu.Unlock()
				return c.detail, c.err
			}
		default:
			d.mu.Unlock()
			return wait(ctx, c)
		}
	}
	if d.total >= detailInFlight || d.perNode[nodeUUID] >= detailPerNode {
		d.mu.Unlock()
		return proto.CardCheckDetail{}, ErrBusy
	}
	c := &detailCall{done: make(chan struct{})}
	d.calls[key] = c
	d.total++
	d.perNode[nodeUUID]++
	d.mu.Unlock()

	go func() {
		callCtx, cancel := context.WithTimeout(context.Background(), detailTimeout)
		defer cancel()
		raw, err := d.call(callCtx, nodeUUID, proto.RPCNodeCardCheckDetail, proto.CardCheckDetailRequest{Ident: ident, Check: check})
		if err == nil {
			err = json.Unmarshal(raw, &c.detail)
		}
		d.mu.Lock()
		c.err, c.at = err, d.now()
		d.total--
		if d.perNode[nodeUUID]--; d.perNode[nodeUUID] <= 0 {
			delete(d.perNode, nodeUUID)
		}
		if err != nil {
			// A failure is not kept: the next ask tries again.
			delete(d.calls, key)
		}
		d.prune()
		d.mu.Unlock()
		close(c.done)
	}()
	return wait(ctx, c)
}

func wait(ctx context.Context, c *detailCall) (proto.CardCheckDetail, error) {
	select {
	case <-c.done:
		return c.detail, c.err
	case <-ctx.Done():
		return proto.CardCheckDetail{}, ctx.Err()
	}
}

// prune drops answers older than the cache, so the map holds only what is in
// flight or fresh.
func (d *Details) prune() {
	now := d.now()
	for key, c := range d.calls {
		select {
		case <-c.done:
			if now.Sub(c.at) >= detailCacheFor {
				delete(d.calls, key)
			}
		default:
		}
	}
}
