package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/servingdial"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// servingDialer opens the direct path to an instance through its node's
// serving forwarder and reports, on change only, whether the node's serving
// network is reachable. There is no fallback: a node with a serving network
// serves over it or not at all.
type servingDialer struct {
	log    *slog.Logger
	dial   servingdial.DialFunc
	now    func() time.Time
	record func(nodeUUID string, st store.ServingForwardStatus)

	mu   sync.Mutex
	last map[string]string // node -> mode+reason last recorded
}

func newServingDialer(log *slog.Logger, record func(string, store.ServingForwardStatus)) *servingDialer {
	return &servingDialer{log: log, dial: servingdial.Dialer(), now: time.Now, record: record, last: map[string]string{}}
}

// open returns a connection spliced to the instance over the serving network.
func (d *servingDialer) open(ctx context.Context, ep network.ServingEndpoint, nodeUUID, instanceUUID string) (net.Conn, error) {
	conn, err := servingdial.Connect(ctx, d.dial, ep.IP, ep.NodeToken, instanceUUID)
	if err != nil {
		d.log.Warn("serving network unreachable", "node", nodeUUID, "addr", ep.IP, "err", err)
		d.report(nodeUUID, store.ServingForwardUnreachable, err.Error())
		return nil, fmt.Errorf("serving network %s unreachable: %w", ep.IP, err)
	}
	d.report(nodeUUID, store.ServingForwardDirect, "")
	return conn, nil
}

func (d *servingDialer) report(nodeUUID, mode, reason string) {
	if d.record == nil {
		return
	}
	key := mode + "\x00" + reason
	d.mu.Lock()
	unchanged := d.last[nodeUUID] == key
	d.last[nodeUUID] = key
	d.mu.Unlock()
	if !unchanged {
		d.record(nodeUUID, store.ServingForwardStatus{Mode: mode, Reason: reason, At: d.now().UTC()})
	}
}
