package app

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/proto"
)

// go test ./internal/agent/app -run '^TestModelSyncStopDuringTransferSuppressesFinalProgress$' -count=1
func TestModelSyncStopDuringTransferSuppressesFinalProgress(t *testing.T) {
	node := &NodeRuntime{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: t.TempDir(),
		modelSyncs: map[string]*activeModelSync{}, progress: newProgressGate(),
	}
	progress := make(chan struct{}, 8)
	late := make(chan proto.ModelFileStatus, 32)
	var stopped atomic.Bool
	node.Bind(func(kind string, data any) error {
		if kind == proto.TStatusModelFile {
			st := data.(proto.ModelFileStatus)
			if stopped.Load() {
				late <- st
			}
			if st.BytesDone > 0 {
				progress <- struct{}{}
			}
		}
		return nil
	})
	params := proto.ModelSyncListenParams{Key: "1", TransferID: "active-transfer", TargetPath: "model", Token: "secret", BindIP: "127.0.0.1"}
	raw, err := node.rpcModelSyncListen(mustRaw(t, params))
	if err != nil {
		t.Fatal(err)
	}
	listen := raw.(proto.ModelSyncListenResult)
	stop := mustRaw(t, proto.ModelSyncStopParams{Key: params.Key, TransferID: params.TransferID})
	t.Cleanup(func() { _, _ = node.rpcModelSyncStop(stop) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		_, err := agentmodelsync.Send(ctx, listen.Endpoints, nil, "secret", listen.Fingerprint, func(w io.Writer) error {
			archive := tar.NewWriter(w)
			if err := archive.WriteHeader(&tar.Header{Name: "model.safetensors", Mode: 0o644, Size: 16 << 20}); err != nil {
				return err
			}
			if _, err := archive.Write(bytes.Repeat([]byte("w"), 9<<20)); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		})
		sent <- err
	}()
	select {
	case <-progress:
	case <-ctx.Done():
		t.Fatal("receiver never reported active transfer progress")
	}
	if _, err := node.rpcModelSyncStop(stop); err != nil {
		t.Fatal(err)
	}
	stopped.Store(true)
	cancel()
	if err := <-sent; err == nil {
		t.Fatal("interrupted archive unexpectedly completed")
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		node.syncMu.Lock()
		active := node.modelSyncs[params.Key] != nil
		node.syncMu.Unlock()
		if !active {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("canceled receiver did not release its task")
		}
	}
	select {
	case st := <-late:
		t.Fatalf("status emitted after cancellation returned: %+v", st)
	default:
	}
	if node.progress.tracked() != 0 {
		t.Fatal("canceled transfer retained buffered progress")
	}
}

// go test ./internal/agent/app -run '^TestModelSyncStopSuppressesStatus$' -count=1
func TestModelSyncStopSuppressesStatus(t *testing.T) {
	for _, phase := range []string{"before-connect", "reading-header", "received"} {
		t.Run(phase, func(t *testing.T) {
			node := &NodeRuntime{
				log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				modelSyncs: map[string]*activeModelSync{},
			}
			clock := newFakeGateClock(time.Unix(100, 0))
			node.progress = newTestGate(clock)
			var statuses []proto.ModelFileStatus
			node.Bind(func(kind string, data any) error {
				if kind == proto.TStatusModelFile {
					statuses = append(statuses, data.(proto.ModelFileStatus))
				}
				return nil
			})
			receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{
				BindHost: "127.0.0.1", Token: "secret",
				Receive: func(_ context.Context, r io.Reader) error {
					_, err := io.Copy(io.Discard, r)
					return err
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(receiver.Stop)
			active := &activeModelSync{transferID: "transfer", key: "1", receiver: receiver}
			node.modelSyncs[active.key] = active
			target := filepath.Join(t.TempDir(), "model")
			if phase == "reading-header" {
				// The ephemeral certificate belongs to this local test receiver.
				conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", receiver.Endpoints[0], &tls.Config{InsecureSkipVerify: true})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if _, err := io.WriteString(conn, `{"magic":`); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "received" {
				writeModel(t, target, "weights")
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if _, err := agentmodelsync.Send(ctx, receiver.Endpoints, nil, "secret", receiver.Fingerprint, func(w io.Writer) error {
					_, err := io.WriteString(w, "completed payload")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			progress := proto.ModelFileStatus{Key: active.key, State: proto.MFSyncing, TransferID: active.transferID}
			node.reportModelFile(progress)
			progress.BytesDone = 100
			node.reportModelFile(progress)
			if clock.armed() != 1 {
				t.Fatal("expected buffered progress before cancellation")
			}
			statuses = nil
			params := mustRaw(t, proto.ModelSyncStopParams{Key: active.key, TransferID: active.transferID})
			result, err := node.rpcModelSyncStop(params)
			if err != nil || result.(map[string]string)["status"] != "stopping" {
				t.Fatalf("stop = %v, %v", result, err)
			}
			node.waitModelSync(active, proto.ModelSyncListenParams{Key: active.key, TransferID: active.transferID}, target)
			clock.advance(time.Second)
			clock.fireDue()
			if len(statuses) != 0 {
				t.Errorf("canceled transfer emitted status: %+v", statuses)
			}
			if len(node.modelSyncs) != 0 || node.progress.tracked() != 0 {
				t.Fatal("canceled transfer retained its task or progress slot")
			}
			result, err = node.rpcModelSyncStop(params)
			if err != nil || result.(map[string]string)["status"] != "not_running" {
				t.Fatalf("repeated stop = %v, %v", result, err)
			}
		})
	}
}

// go test ./internal/agent/app -run '^TestModelSyncValidationFailureWithoutCancelIsReported$' -count=1
func TestModelSyncValidationFailureWithoutCancelIsReported(t *testing.T) {
	node := &NodeRuntime{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		modelSyncs: map[string]*activeModelSync{},
	}
	var statuses []proto.ModelFileStatus
	node.Bind(func(kind string, data any) error {
		if kind == proto.TStatusModelFile {
			statuses = append(statuses, data.(proto.ModelFileStatus))
		}
		return nil
	})
	receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{
		BindHost: "127.0.0.1", Token: "secret",
		Receive: func(_ context.Context, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Stop)
	active := &activeModelSync{transferID: "current", key: "1", receiver: receiver}
	node.modelSyncs[active.key] = active
	if _, err := node.rpcModelSyncStop(mustRaw(t, proto.ModelSyncStopParams{Key: active.key, TransferID: "old"})); err == nil {
		t.Fatal("stop accepted a different transfer ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := agentmodelsync.Send(ctx, receiver.Endpoints, nil, "secret", receiver.Fingerprint, func(w io.Writer) error {
		_, err := io.WriteString(w, "payload")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Transport completes, but the target has no model files to validate.
	target := t.TempDir()
	_, validationErr := validateModelDir(target)
	if validationErr == nil {
		t.Fatal("fixture unexpectedly contains a valid model")
	}
	node.waitModelSync(active, proto.ModelSyncListenParams{Key: active.key, TransferID: active.transferID}, target)
	if len(statuses) != 1 || statuses[0].State != proto.MFFailed || statuses[0].Message != boundedModelSyncMessage(validationErr) {
		t.Fatalf("real validation failure was hidden: %+v", statuses)
	}
}

// go test ./internal/agent/app -run '^TestModelSyncStopDoesNotWaitForStatusSend$' -count=1
func TestModelSyncStopDoesNotWaitForStatusSend(t *testing.T) {
	node := &NodeRuntime{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: t.TempDir(),
		modelSyncs: map[string]*activeModelSync{}, progress: newProgressGate(),
	}
	// Deliver progress immediately so this case controls the actual send boundary.
	node.progress.interval = 0
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	releaseSend := func() { unblock.Do(func() { close(release) }) }
	defer releaseSend()
	var failed atomic.Int64
	node.Bind(func(kind string, data any) error {
		if kind == proto.TStatusModelFile {
			st := data.(proto.ModelFileStatus)
			if st.State == proto.MFFailed {
				failed.Add(1)
			}
			if st.BytesDone > 0 {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-release
			}
		}
		return nil
	})
	params := proto.ModelSyncListenParams{Key: "1", TransferID: "blocked-status", TargetPath: "model", Token: "secret", BindIP: "127.0.0.1"}
	raw, err := node.rpcModelSyncListen(mustRaw(t, params))
	if err != nil {
		t.Fatal(err)
	}
	listen := raw.(proto.ModelSyncListenResult)
	stopParams := mustRaw(t, proto.ModelSyncStopParams{Key: params.Key, TransferID: params.TransferID})
	t.Cleanup(func() { _, _ = node.rpcModelSyncStop(stopParams) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		_, err := agentmodelsync.Send(ctx, listen.Endpoints, nil, params.Token, listen.Fingerprint, func(w io.Writer) error {
			archive := tar.NewWriter(w)
			if err := archive.WriteHeader(&tar.Header{Name: "model.safetensors", Mode: 0o644, Size: 16 << 20}); err != nil {
				return err
			}
			if _, err := archive.Write(bytes.Repeat([]byte("w"), 9<<20)); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		})
		sent <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("receiver never reached the blocked status send")
	}
	type stopResponse struct {
		result any
		err    error
	}
	stopped := make(chan stopResponse, 1)
	go func() { result, err := node.rpcModelSyncStop(stopParams); stopped <- stopResponse{result, err} }()
	// The timeout bounds a failed test; successful cancellation must happen while
	// the send remains blocked, independently of network recovery.
	select {
	case response := <-stopped:
		if response.err != nil || response.result.(map[string]string)["status"] != "stopping" {
			t.Errorf("stop = %v, %v", response.result, response.err)
		}
		conn, err := net.DialTimeout("tcp", listen.Endpoints[0], time.Second)
		if err == nil {
			conn.Close()
			t.Error("canceled receiver is still accepting connections")
		}
	case <-time.After(time.Second):
		t.Error("cancellation waited for the blocked status send")
		releaseSend()
		<-stopped
	}
	releaseSend()
	cancel()
	if err := <-sent; err == nil {
		t.Error("interrupted archive unexpectedly completed")
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		node.syncMu.Lock()
		active := node.modelSyncs[params.Key] != nil
		node.syncMu.Unlock()
		if !active {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("canceled receiver did not release its task")
		}
	}
	if failed.Load() != 0 {
		t.Error("canceled receiver reported failed")
	}
	if node.progress.tracked() != 0 {
		t.Error("canceled receiver retained buffered progress")
	}
}
