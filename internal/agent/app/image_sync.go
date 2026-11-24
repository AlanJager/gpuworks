package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	imageSyncMagic             = "GPUWORKS-IMAGE/1"
	imageArchiveMaxBytes int64 = 500 << 30
)

type activeImageSync struct {
	transferID string
	imageRef   string
	receiver   *agentmodelsync.Receiver
	reported   bool
	relayDone  chan struct{}
}

type readWriteReader struct{ io.Reader }

func (readWriteReader) Write(p []byte) (int, error) {
	return 0, errors.New("image receiver is read-only")
}

func (i *nodeImages) rpcImageSyncListen(_ context.Context, params json.RawMessage) (any, error) {
	var p proto.ImageSyncListenParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	p.TransferID, p.ImageRef, p.Token = strings.TrimSpace(p.TransferID), strings.TrimSpace(p.ImageRef), strings.TrimSpace(p.Token)
	if p.TransferID == "" || p.ImageRef == "" || p.Token == "" {
		return nil, errors.New("image sync listen requires transfer_id, image_ref, and token")
	}
	if p.ExpectedBytes < 0 || p.ExpectedBytes > imageArchiveMaxBytes {
		return nil, fmt.Errorf("image archive exceeds %d-byte limit", imageArchiveMaxBytes)
	}
	d := i.deps.driver()
	if _, ok := d.(runtime.ImageArchiver); !ok {
		return nil, fmt.Errorf("runtime %s does not support direct image save/load", i.driverName())
	}
	if p.ExpectedBytes > 0 {
		if err := checkModelDownloadSpace("/", p.ExpectedBytes); err != nil {
			return nil, fmt.Errorf("target disk precheck failed: %w", err)
		}
	}
	i.syncMu.Lock()
	if current := i.syncs[p.ImageRef]; current != nil {
		i.syncMu.Unlock()
		return proto.ImageSyncListenResult{InProgress: true, TransferID: current.transferID}, nil
	}
	i.syncMu.Unlock()
	bindHost, advertise, err := agentmodelsync.ListenAddrs(p.BindIP)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	active := &activeImageSync{transferID: p.TransferID, imageRef: p.ImageRef}
	receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{
		BindHost: bindHost, AdvertiseHosts: advertise, Token: p.Token, Magic: imageSyncMagic,
		MaxBytes: imageArchiveMaxBytes,
		Receive: func(ctx context.Context, r io.Reader) error {
			return i.streamImage(ctx, p.ImageRef, p.TransferID, readWriteReader{Reader: r}, true, func() {
				i.syncMu.Lock()
				active.reported = true
				if i.syncs[p.ImageRef] == active {
					delete(i.syncs, p.ImageRef)
				}
				i.syncMu.Unlock()
			})
		},
		Progress: func(done int64) {
			// The receiver flushes its final byte count after Receive returns,
			// which is after StreamImage already reported the terminal state.
			// A late "syncing" frame would reopen a finished row.
			i.syncMu.Lock()
			reported := active.reported
			i.syncMu.Unlock()
			if reported {
				return
			}
			seconds := time.Since(started).Seconds()
			rate := int64(0)
			if seconds > 0 {
				rate = int64(float64(done) / seconds)
			}
			i.deps.report(proto.ImageNodeStatus{ImageRef: p.ImageRef, State: "syncing", SizeBytes: p.ExpectedBytes, TransferredBytes: done, RateBPS: rate, TransferID: p.TransferID, Source: "peer"})
		},
	})
	if err != nil {
		return nil, err
	}
	active.receiver = receiver
	i.syncMu.Lock()
	if current := i.syncs[p.ImageRef]; current != nil {
		i.syncMu.Unlock()
		receiver.Stop()
		return proto.ImageSyncListenResult{InProgress: true, TransferID: current.transferID}, nil
	}
	i.syncs[p.ImageRef] = active
	i.syncMu.Unlock()
	go i.waitImageSync(active, p)
	return proto.ImageSyncListenResult{Endpoints: receiver.Endpoints, Fingerprint: receiver.Fingerprint, TransferID: p.TransferID}, nil
}

func (i *nodeImages) waitImageSync(active *activeImageSync, p proto.ImageSyncListenParams) {
	_, err := active.receiver.Wait(context.Background())
	i.syncMu.Lock()
	if i.syncs[p.ImageRef] == active {
		delete(i.syncs, p.ImageRef)
	}
	i.syncMu.Unlock()
	i.syncMu.Lock()
	reported := active.reported
	i.syncMu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) && !reported {
		i.deps.report(proto.ImageNodeStatus{ImageRef: p.ImageRef, State: "failed", TransferID: p.TransferID, Source: "peer", Message: err.Error()})
	}
}

func (i *nodeImages) rpcImageSyncSend(ctx context.Context, params json.RawMessage) (any, error) {
	return i.imageSyncSend(ctx, params, nil)
}

func (i *nodeImages) imageSyncSend(ctx context.Context, params json.RawMessage, raw net.Conn) (any, error) {
	var p proto.ImageSyncSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	p.TransferID = strings.TrimSpace(p.TransferID)
	p.ImageRef, p.Token, p.Fingerprint = strings.TrimSpace(p.ImageRef), strings.TrimSpace(p.Token), strings.TrimSpace(p.Fingerprint)
	if p.ImageRef == "" || p.Token == "" || p.Fingerprint == "" || len(p.Endpoints) == 0 {
		return nil, errors.New("image sync send parameters are incomplete")
	}
	if p.RateLimitKBs < 0 {
		return nil, errors.New("image sync rate limit must be non-negative")
	}
	if p.TransferID == "" {
		return nil, errors.New("image sync send requires transfer_id")
	}
	ctx, cancel := context.WithCancel(ctx)
	i.syncMu.Lock()
	if _, exists := i.sends[p.TransferID]; exists {
		i.syncMu.Unlock()
		cancel()
		return nil, errors.New("image sync sender already owns this transfer_id")
	}
	i.sends[p.TransferID] = cancel
	i.syncMu.Unlock()
	defer func() {
		cancel()
		i.syncMu.Lock()
		delete(i.sends, p.TransferID)
		i.syncMu.Unlock()
	}()
	d, ok := i.deps.driver().(runtime.ImageArchiver)
	if !ok {
		return nil, fmt.Errorf("runtime %s does not support direct image save/load", i.driverName())
	}
	localIP, err := agentmodelsync.LocalIP(p.SourceIP)
	if err != nil {
		return nil, err
	}
	transport := "direct"
	if raw != nil {
		transport = "relay"
	}
	observe := i.deps.phaseReporter(p.ReportPhase, p.TransferID, transport)
	writeArchive := func(w io.Writer) error {
		return d.SaveImage(ctx, p.ImageRef, agentmodelsync.RateLimitedWriter(ctx, w, p.RateLimitKBs))
	}
	var result agentmodelsync.Result
	if raw != nil {
		result, err = agentmodelsync.SendOnConnWithMagic(ctx, raw, p.Token, p.Fingerprint, imageSyncMagic, writeArchive, observe)
	} else {
		result, err = agentmodelsync.SendWithMagic(ctx, p.Endpoints, localIP, p.Token, p.Fingerprint, imageSyncMagic, writeArchive, observe)
	}
	if err != nil {
		if p.AllowRelay && errors.Is(err, agentmodelsync.ErrDirectUnavailable) {
			return proto.ImageSyncTransferResult{DirectDialError: err.Error()}, nil
		}
		return nil, err
	}
	return proto.ImageSyncTransferResult{Bytes: result.Bytes, SHA256: result.SHA256}, nil
}

func (i *nodeImages) rpcImageSyncProbe(ctx context.Context, params json.RawMessage) (any, error) {
	var p proto.ImageSyncSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Token) == "" || strings.TrimSpace(p.Fingerprint) == "" || len(p.Endpoints) == 0 {
		return nil, errors.New("image sync probe parameters are incomplete")
	}
	localIP, err := agentmodelsync.LocalIP(p.SourceIP)
	if err != nil {
		return nil, err
	}
	if err := agentmodelsync.Probe(ctx, p.Endpoints, localIP, p.Token, p.Fingerprint, imageSyncMagic); err != nil {
		if p.AllowRelay && errors.Is(err, agentmodelsync.ErrDirectUnavailable) {
			return proto.ImageSyncTransferResult{DirectDialError: err.Error()}, nil
		}
		return nil, fmt.Errorf("source node cannot connect to target direct address: %w", err)
	}
	return map[string]string{"state": "ready"}, nil
}

func (i *nodeImages) rpcImageSyncStop(_ context.Context, params json.RawMessage) (any, error) {
	var p proto.ImageSyncStopParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.TransferID) == "" {
		return nil, errors.New("image sync stop requires transfer_id")
	}
	i.syncMu.Lock()
	if cancel := i.sends[p.TransferID]; cancel != nil {
		i.syncMu.Unlock()
		cancel()
		return map[string]string{"status": "stopping"}, nil
	}
	var active *activeImageSync
	for _, candidate := range i.syncs {
		if p.TransferID == "" || candidate.transferID == p.TransferID {
			active = candidate
			break
		}
	}
	i.syncMu.Unlock()
	if active == nil {
		return map[string]string{"status": "not_running"}, nil
	}
	i.syncMu.Lock()
	active.reported = true
	i.syncMu.Unlock()
	active.receiver.Stop()
	return map[string]string{"status": "stopping"}, nil
}

// attachRelay hands the peer relay the private listener of a live transfer
// that no relay is attached to yet, with the function that detaches it. Both
// are nil when there is no such transfer.
func (i *nodeImages) attachRelay(transferID string) (*agentmodelsync.Receiver, func()) {
	i.syncMu.Lock()
	defer i.syncMu.Unlock()
	for _, active := range i.syncs {
		if active.transferID == transferID && !active.reported && active.relayDone == nil && active.receiver != nil {
			done := make(chan struct{})
			active.relayDone = done
			return active.receiver, func() { i.syncMu.Lock(); active.relayDone = nil; close(done); i.syncMu.Unlock() }
		}
	}
	return nil, nil
}

// relayAttached is closed when the relay attached to transferID detaches; nil
// when none is attached.
func (i *nodeImages) relayAttached(transferID string) <-chan struct{} {
	i.syncMu.Lock()
	defer i.syncMu.Unlock()
	for _, active := range i.syncs {
		if active.transferID == transferID {
			return active.relayDone
		}
	}
	return nil
}

// relaySend sends an image over a relayed connection when the server allowed
// the transfer to be relayed.
func (i *nodeImages) relaySend(ctx context.Context, params json.RawMessage, raw net.Conn) (any, error) {
	var p proto.ImageSyncSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if !p.AllowRelay {
		return nil, errors.New("image relay is not authorized")
	}
	return i.imageSyncSend(ctx, params, raw)
}

// relayProbe checks a relayed connection reaches the receiving node's
// listener when the server allowed the transfer to be relayed.
func (i *nodeImages) relayProbe(ctx context.Context, params json.RawMessage, raw net.Conn) (any, error) {
	var p proto.ImageSyncSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if !p.AllowRelay {
		return nil, errors.New("peer relay is not authorized")
	}
	if err := agentmodelsync.ProbeOnConnWithMagic(ctx, raw, p.Token, p.Fingerprint, imageSyncMagic); err != nil {
		return nil, err
	}
	return map[string]string{"state": "ready"}, nil
}
