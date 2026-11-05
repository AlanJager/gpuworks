// pd_bootstrap.go reads the engine id a synthesized handoff names the prefill
// instance by (pdmode.Mode.SynthesizedHandoff, vLLM's MooncakeConnector):
// the engine id its workers registered with its bootstrap server.

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// bootstrapEngineTTL bounds how long a prefill instance's engine id is
// reused without asking its bootstrap server again. An instance keeps its
// engine id for its whole life (an engine that restarts is a new instance),
// so the bound only keeps the cache from holding instances long gone.
const bootstrapEngineTTL = 10 * time.Minute

// maxBootstrapAnswerBytes bounds a bootstrap server's /query answer: one
// address per worker.
const maxBootstrapAnswerBytes = 1 << 20

// bootstrapEngines caches, per prefill instance, the engine id its bootstrap
// server reports once every TP rank has registered.
type bootstrapEngines struct {
	mu      sync.Mutex
	entries map[string]bootstrapEngine
}

type bootstrapEngine struct {
	id string
	at time.Time
}

func (b *bootstrapEngines) get(uuid string, now time.Time) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[uuid]
	if !ok || now.Sub(entry.at) > bootstrapEngineTTL {
		return "", false
	}
	return entry.id, true
}

func (b *bootstrapEngines) put(uuid, id string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[string]bootstrapEngine{}
	}
	for key, entry := range b.entries {
		if now.Sub(entry.at) > bootstrapEngineTTL {
			delete(b.entries, key)
		}
	}
	b.entries[uuid] = bootstrapEngine{id: id, at: now}
}

// errBootstrapIncomplete is a bootstrap server that answers before every TP
// rank of its instance has registered: a decoder told to pull from it now
// would miss the ranks still to come.
var errBootstrapIncomplete = errors.New("not every TP rank has registered with the bootstrap server")

// bootstrapAnswer is the bootstrap server's /query answer: per data parallel
// rank, the engine id and the address of every (TP rank, PP rank) worker.
type bootstrapAnswer map[string]struct {
	EngineID   string                       `json:"engine_id"`
	WorkerAddr map[string]map[string]string `json:"worker_addr"`
}

// bootstrapEngineID returns the engine id a prefill instance's workers
// registered with its bootstrap server, asking the server when it is not
// cached. tp is the prefill role's tensor parallel degree. The question goes
// through the agent tunnel even on a node with a serving network: the
// serving forwarder reaches an instance's API port only, and this is one
// small request per instance.
func (p *Proxy) bootstrapEngineID(ctx context.Context, c candidate, tp int) (string, error) {
	if c.kvPort == 0 {
		return "", errors.New("prefill instance has no bootstrap port")
	}
	if id, ok := p.bootstrap.get(c.instanceUUID, time.Now()); ok {
		return id, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/query", c.kvPort), nil)
	if err != nil {
		return "", err
	}
	tunnel := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.hub.OpenTunnel(ctx, c.nodeUUID, fmt.Sprintf("127.0.0.1:%d", c.kvPort))
		},
		DisableKeepAlives: true,
	}
	resp, err := tunnel.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bootstrap server answered %d", resp.StatusCode)
	}
	var answer bootstrapAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBootstrapAnswerBytes)).Decode(&answer); err != nil {
		return "", fmt.Errorf("bootstrap answer: %w", err)
	}
	entry, ok := answer["0"]
	if !ok || entry.EngineID == "" {
		return "", errBootstrapIncomplete
	}
	if len(entry.WorkerAddr) < max(tp, 1) {
		return "", errBootstrapIncomplete
	}
	p.bootstrap.put(c.instanceUUID, entry.EngineID, time.Now())
	return entry.EngineID, nil
}
