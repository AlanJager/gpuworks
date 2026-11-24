package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/proto"
)

// StreamPeerSend retains the direct protocol and reports its result on ctrl.
func (n *NodeRuntime) StreamPeerSend(ctx context.Context, method string, params json.RawMessage, raw net.Conn) (any, error) {
	if raw == nil {
		return nil, errors.New("peer relay connection is required")
	}
	defer raw.Close()
	switch method {
	case proto.RPCModelSyncSend:
		var p proto.ModelSyncSendParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if !p.AllowRelay {
			return nil, errors.New("model relay is not authorized")
		}
		return n.modelSyncSend(ctx, params, raw)
	case proto.RPCImageSyncSend:
		return n.imageWork().relaySend(ctx, params, raw)
	case proto.RPCImageSyncProbe:
		return n.imageWork().relayProbe(ctx, params, raw)
	case proto.RPCModelSyncProbe:
		var p proto.ModelSyncSendParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if !p.AllowRelay {
			return nil, errors.New("peer relay is not authorized")
		}
		if err := agentmodelsync.ProbeOnConnWithMagic(ctx, raw, p.Token, p.Fingerprint, "GPUWORKS-MODEL/1"); err != nil {
			return nil, err
		}
		return map[string]string{"state": "ready"}, nil
	default:
		return nil, errors.New("unsupported peer relay method")
	}
}

type peerReceiverConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *peerReceiverConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func (c *peerReceiverConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return nil
}

// PeerRelayReceiver resolves only a live transfer's private local listener.
// The caller supplies no network destination and cannot turn this into a proxy.
func (n *NodeRuntime) PeerRelayReceiver(ctx context.Context, kind, transferID string) (net.Conn, error) {
	if transferID == "" {
		return nil, errors.New("peer relay requires transfer_id")
	}
	var receiver *agentmodelsync.Receiver
	var release func()
	switch kind {
	case "model":
		n.syncMu.Lock()
		for _, active := range n.modelSyncs {
			if active.transferID == transferID && !active.canceled.Load() && active.relayDone == nil && active.receiver != nil {
				done := make(chan struct{})
				active.relayDone = done
				receiver = active.receiver
				release = func() { n.syncMu.Lock(); active.relayDone = nil; close(done); n.syncMu.Unlock() }
				break
			}
		}
		n.syncMu.Unlock()
	case "image":
		receiver, release = n.imageWork().attachRelay(transferID)
	default:
		return nil, errors.New("unsupported peer relay receiver kind")
	}
	if receiver == nil {
		return nil, errors.New("peer relay transfer is unavailable or already attached")
	}
	raw, err := receiver.DialLocal(ctx)
	if err != nil {
		release()
		return nil, err
	}
	return &peerReceiverConn{Conn: raw, release: release}, nil
}

// rpcPeerRelayWaitClosed acknowledges that the target stream has released its
// transfer attachment. This makes a following probe/send independent of wire
// scheduling between the control and data lanes.
func (n *NodeRuntime) rpcPeerRelayWaitClosed(ctx context.Context, params json.RawMessage) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var p proto.PeerRelayWaitClosedParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.TransferID == "" {
		return nil, errors.New("peer relay wait requires transfer_id")
	}
	var done <-chan struct{}
	switch p.Kind {
	case "model":
		n.syncMu.Lock()
		for _, active := range n.modelSyncs {
			if active.transferID == p.TransferID {
				done = active.relayDone
				break
			}
		}
		n.syncMu.Unlock()
	case "image":
		done = n.imageWork().relayAttached(p.TransferID)
	default:
		return nil, errors.New("unsupported peer relay receiver kind")
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return proto.PeerRelayReady{}, nil
}
