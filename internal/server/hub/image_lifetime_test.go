package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStoppedHubDoesNotAdvanceImageWatchesAndCanReenter(t *testing.T) {
	h, image, _, node := imageStallFixture(t, "image_epoch_watch")
	previousCheck, previousAfter := imageDirectStallCheck, imageDirectStallAfter
	imageDirectStallCheck, imageDirectStallAfter = 5*time.Millisecond, time.Minute
	defer func() { imageDirectStallCheck, imageDirectStallAfter = previousCheck, previousAfter }()
	defer h.StopSessions()
	initial, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: node.ID, State: store.ImageStateSyncing, Source: "url", TransferID: "kept"})
	if err != nil {
		t.Fatal(err)
	}
	h.WatchImageTransfer(image.ID, node.ID)
	h.StopSessions()
	// A stopped epoch must not observe a later change or recreate a watcher.
	h.WatchImageTransfer(image.ID, node.ID)
	if err := h.store.DB().Model(&store.ImageNode{}).Where("id = ?", initial.ID).Update("updated_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	row, err := h.store.ImageNode(image.ID, node.ID)
	if err != nil || row.State != store.ImageStateSyncing || row.TransferID != "kept" {
		t.Fatalf("stopped image watcher changed persistent state: %+v %v", row, err)
	}
	if _, err := h.PullImage(context.Background(), image.ID, node.UUID, imagecatalog.FetchRequest{}); !errors.Is(err, errSessionsStopped) {
		t.Fatalf("pull admitted after stop: %v", err)
	}
	if _, err := h.SyncImageFrom(context.Background(), image.ID, node.UUID, "source"); !errors.Is(err, errSessionsStopped) {
		t.Fatalf("sync admitted after stop: %v", err)
	}
	h.StartSessions()
	h.WatchImageTransfer(image.ID, node.ID)
	waitFor(t, time.Second, func() bool {
		row, err := h.store.ImageNode(image.ID, node.ID)
		return err == nil && row.State == store.ImageStateFailed
	}, "new epoch to watch the persisted attempt")
}

func TestStopSessionsCancelsDetachedArchiveRelay(t *testing.T) {
	h := newRelayHub(t, "file:image_epoch_relay?mode=memory&cache=shared", "/image.tar")
	defer h.hub.StopSessions()
	entered, cancelled := make(chan struct{}), make(chan struct{})
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(h.archive)))
			return
		}
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer func() {
		site.CloseClientConnections()
		site.Close()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	fetch := imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceURL, ArchiveURL: site.URL + "/image.tar", Relay: true}
	row, err := h.hub.PullImage(ctx, h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("relay did not issue its HTTP download")
	}
	cancel() // Returning HTTP callers do not own the durable transfer.
	select {
	case <-cancelled:
		t.Fatal("caller cancellation killed a detached transfer")
	case <-time.After(20 * time.Millisecond):
	}
	h.hub.StopSessions()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Hub stop left its archive HTTP request running")
	}
	final, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil || final.State != store.ImageStateFailed || final.LastTransferID != row.TransferID {
		t.Fatalf("interrupted relay was not finalized before stop: %+v %v", final, err)
	}
}

func TestStoppedHubCancelsArchiveConfirmationWithoutLateFailure(t *testing.T) {
	previous := imageTransferConfirmWait
	imageTransferConfirmWait = 80 * time.Millisecond
	defer func() { imageTransferConfirmWait = previous }()
	h := newRelayHub(t, "file:image_epoch_confirmation?mode=memory&cache=shared", "/image.tar")
	defer h.hub.StopSessions()
	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceURL, ArchiveURL: h.url, Relay: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.loaded:
	case <-time.After(time.Second):
		t.Fatal("archive was not delivered")
	}
	waitFor(t, time.Second, func() bool {
		current, err := h.store.ImageNode(h.image.ID, h.node.ID)
		return err == nil && current.TransferredBytes == int64(len(h.archive))
	}, "relay final progress")
	h.hub.StopSessions()
	time.Sleep(2 * imageTransferConfirmWait)
	current, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil || current.State != store.ImageStateSyncing || current.TransferID != row.TransferID {
		t.Fatalf("stopped confirmation timer rewrote persisted attempt: %+v %v", current, err)
	}
}

type imageEpochNetworks struct{}

func (imageEpochNetworks) StoragePath(uint, uint) (netplan.StoragePath, error) {
	return netplan.StoragePath{SourceIP: "192.0.2.1", TargetIP: "192.0.2.2"}, nil
}
func (imageEpochNetworks) ServingIP(uint) string { return "" }

func TestStopSessionsJoinsDirectImageCopyAndPreservesOperatorCancel(t *testing.T) {
	h, image, source, target := imageStallFixture(t, "image_epoch_copy")
	h.SetNetworks(imageEpochNetworks{})
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: source.ID, State: store.ImageStatePresent, SizeBytes: 42}); err != nil {
		t.Fatal(err)
	}
	sending := make(chan struct{}, 4)
	for _, node := range []*store.Node{source, target} {
		s := newSession(h, nil, nil)
		s.NodeUUID = node.UUID
		s.sendFn = func(kind string, id uint64, value any) error {
			request := value.(rpcRequest)
			response := rpcResponse{}
			switch request.Method {
			case proto.RPCImageSyncListen:
				response.Result = json.RawMessage(`{"endpoints":["192.0.2.2:12001"],"fingerprint":"fixture-fingerprint"}`)
			case proto.RPCImageSyncSend:
				sending <- struct{}{}
				return nil // The source acknowledges receipt but has no archive verdict yet.
			case proto.RPCImageSyncStop:
				response.Result = json.RawMessage(`{}`)
			default:
				response.Error = "unknown method"
			}
			body, _ := json.Marshal(response)
			s.dispatchRPCResp(proto.Envelope{T: proto.TRPCResp, ID: id, D: body})
			return nil
		}
		h.sessions[node.UUID] = s
	}
	defer h.StopSessions()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = h.CancelImageTransfer(ctx, image.ID, target.UUID)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	first, err := h.SyncImageFrom(ctx, image.ID, target.UUID, source.UUID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sending:
	case <-time.After(time.Second):
		t.Fatal("direct copy did not reach the selected source")
	}
	cancel()
	h.StopSessions()
	final, err := h.store.ImageNode(image.ID, target.ID)
	if err != nil || final.State != store.ImageStateFailed || final.LastTransferID != first.TransferID {
		t.Fatalf("direct copy still owns the attempt after stop: %+v %v", final, err)
	}
	h.StartSessions()
	second, err := h.SyncImageFrom(context.Background(), image.ID, target.UUID, source.UUID)
	if err != nil || second.TransferID == first.TransferID {
		t.Fatalf("fresh epoch could not retry: %+v %v", second, err)
	}
	select {
	case <-sending:
	case <-time.After(time.Second):
		t.Fatal("new epoch did not copy")
	}
	cancelled, err := h.CancelImageTransfer(context.Background(), image.ID, target.UUID)
	if err != nil || cancelled.Message != "the transfer was cancelled" {
		t.Fatalf("operator cancellation changed: %+v %v", cancelled, err)
	}
	h.StopSessions()
	final, err = h.store.ImageNode(image.ID, target.ID)
	if err != nil || final.Message != "the transfer was cancelled" {
		t.Fatalf("late copy result overwrote operator cancellation: %+v %v", final, err)
	}
}

func TestStoppedHubCannotAdvanceLegacyImagePullFallback(t *testing.T) {
	requests := make(chan proto.ImagePullParams, 8)
	h := newArchiveURLHub(t, "file:image_epoch_fallback?mode=memory&cache=shared", func(env proto.Envelope) string {
		var request struct {
			Method string                `json:"method"`
			Params proto.ImagePullParams `json:"params"`
		}
		if json.Unmarshal(env.D, &request) != nil || request.Method != proto.RPCImagePull {
			return `{"error":"unknown method"}`
		}
		requests <- request.Params
		return `{"result":{"state":"pulling"}}`
	})
	defer h.hub.StopSessions()
	fetch := imagecatalog.FetchRequest{Copies: []string{"mirror-a.example/runner:v1", "mirror-b.example/runner:v1"}}
	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	<-requests
	h.hub.StopSessions()
	current, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil || current.State != store.ImageStatePulling || current.TransferID != row.TransferID {
		t.Fatalf("server stop was recorded as a fallback timeout: %+v %v", current, err)
	}
	// A later agent/operation result belongs to no old server watcher.
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ID: row.ID, ImageID: h.image.ID, NodeID: h.node.ID, State: store.ImageStateFailed, Message: "after-stop-result"}); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		t.Fatalf("stopped fallback issued a later pull: %+v", request)
	case <-time.After(2200 * time.Millisecond):
	}
	current, err = h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil || current.Message != "after-stop-result" {
		t.Fatalf("old fallback rewrote later state: %+v %v", current, err)
	}
	h.hub.StartSessions()
	again, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil || again.TransferID == row.TransferID {
		t.Fatalf("new epoch could not pull: %+v %v", again, err)
	}
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("new epoch did not contact the agent")
	}
}
