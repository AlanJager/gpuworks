package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

type blockedImageSaver struct {
	*recording.Driver
	entered chan struct{}
}

func (d *blockedImageSaver) SaveImage(ctx context.Context, _ string, _ io.Writer) error {
	close(d.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestImageSyncStopCancelsSourceRuntimeBeforeAnyBytes(t *testing.T) {
	driver := &blockedImageSaver{Driver: recording.New(t.TempDir() + "/spec.json"), entered: make(chan struct{})}
	node := NewNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), driver, t.TempDir()+"/models")
	receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{BindHost: "127.0.0.1", Token: "secret", Magic: imageSyncMagic, Receive: func(_ context.Context, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Stop)
	params, _ := json.Marshal(proto.ImageSyncSendParams{TransferID: "blocked-save", ImageRef: "fixture/source:stall", Token: "secret", Endpoints: receiver.Endpoints, Fingerprint: receiver.Fingerprint})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := node.rpcImageSyncSend(ctx, params); done <- err }()
	select {
	case <-driver.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("source save did not start")
	}
	wrong, _ := json.Marshal(proto.ImageSyncStopParams{TransferID: "another-attempt"})
	result, err := node.rpcImageSyncStop(context.Background(), wrong)
	if err != nil || result.(map[string]string)["status"] != "not_running" {
		t.Fatalf("unrelated stop=%+v err=%v", result, err)
	}
	select {
	case err := <-done:
		t.Fatalf("wrong transfer cancelled source: %v", err)
	default:
	}
	stop, _ := json.Marshal(proto.ImageSyncStopParams{TransferID: "blocked-save"})
	if _, err := node.rpcImageSyncStop(context.Background(), stop); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("source error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("source runtime stayed blocked after stopping transfer")
	}
	node.imageSyncMu.Lock()
	remaining := len(node.imageSyncSends)
	node.imageSyncMu.Unlock()
	if remaining != 0 {
		t.Fatalf("source slot leaked: %d", remaining)
	}
}
