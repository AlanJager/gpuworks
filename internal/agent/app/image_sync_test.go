package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A real direct transfer into the receiver: the terminal status must be the
// last status for the transfer, even though the receiver flushes its final
// byte count after the load has reported.
func TestImageSyncReceiverReportsNothingAfterTerminal(t *testing.T) {
	const ref = "example/peer:late-progress"
	driver := recording.New(t.TempDir() + "/spec.json")
	node := NewNodeRuntime(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), driver, t.TempDir()+"/models")
	statuses := make(chan proto.ImageNodeStatus, 64)
	node.send = func(kind string, data any) error {
		if kind == proto.TStatusImageNode {
			statuses <- data.(proto.ImageNodeStatus)
		}
		return nil
	}
	params, _ := json.Marshal(proto.ImageSyncListenParams{TransferID: "t1", ImageRef: ref, Token: "secret", BindIP: "127.0.0.1"})
	raw, err := node.rpcImageSyncListen(params)
	if err != nil {
		t.Fatal(err)
	}
	listen := raw.(proto.ImageSyncListenResult)
	result, err := agentmodelsync.SendWithMagic(context.Background(), listen.Endpoints, nil, "secret", listen.Fingerprint, imageSyncMagic, func(w io.Writer) error {
		_, err := w.Write([]byte("archive-bytes"))
		return err
	})
	if err != nil || result.Bytes != int64(len("archive-bytes")) {
		t.Fatalf("send result=%+v err=%v", result, err)
	}
	var last proto.ImageNodeStatus
	terminalSeen := false
	deadline := time.After(3 * time.Second)
	for {
		select {
		case st := <-statuses:
			if terminalSeen {
				t.Fatalf("status after terminal: %+v", st)
			}
			last = st
			terminalSeen = st.State == "present" || st.State == "failed"
		case <-deadline:
			if !terminalSeen || last.State != "present" || last.TransferID != "t1" {
				t.Fatalf("last status = %+v, want present for t1", last)
			}
			return
		}
	}
}

func TestImageSyncStopDoesNotReportHeaderReadFailure(t *testing.T) {
	const ref = "example/peer:cancel-header"
	driver := recording.New(t.TempDir() + "/spec.json")
	node := NewNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), driver, t.TempDir()+"/models")
	var statuses []proto.ImageNodeStatus
	node.send = func(kind string, data any) error {
		if kind == proto.TStatusImageNode {
			statuses = append(statuses, data.(proto.ImageNodeStatus))
		}
		return nil
	}
	receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{
		BindHost: "127.0.0.1", Token: "secret", Magic: imageSyncMagic,
		Receive: func(context.Context, io.Reader) error {
			t.Error("incomplete header must not start loading an image")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Stop)
	active := &activeImageSync{transferID: "cancel-header", imageRef: ref, receiver: receiver}
	node.imageSyncs[ref] = active
	// This fixture's ephemeral TLS certificate belongs to the local receiver.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", receiver.Endpoints[0], &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, `{"magic":`); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(proto.ImageSyncStopParams{TransferID: active.transferID})
	if _, err := node.rpcImageSyncStop(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	node.waitImageSync(active, proto.ImageSyncListenParams{ImageRef: ref, TransferID: active.transferID})
	if len(statuses) != 0 {
		t.Fatalf("cancelled transfer emitted statuses that overwrite cancellation: %+v", statuses)
	}
	if len(node.imageSyncs) != 0 {
		t.Fatalf("cancelled transfer still occupies the image sync slot: %+v", node.imageSyncs)
	}
	raw, err := node.rpcImageSyncStop(context.Background(), params)
	if err != nil || raw.(map[string]string)["status"] != "not_running" {
		t.Fatalf("stopping completed cancellation = %+v, %v", raw, err)
	}
}
