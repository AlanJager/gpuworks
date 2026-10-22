package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// pullRecorder records the fetch requests that reach ImageFetch.Pull and the
// peer copies that reach PeerSync, so a route test can assert which path ran.
type pullRecorder struct {
	pulls    []imagecatalog.FetchRequest
	peers    []peerCall
	retries  []string
	imageIDs []uint
}

type peerCall struct {
	imageID    uint
	node, from string
	rate       int64
}

func (r *pullRecorder) deps(st *store.Store) Deps {
	return Deps{
		Store: st,
		ImageFetch: ImageFetchHooks{Pull: func(_ context.Context, imageID uint, nodeUUID string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
			r.pulls = append(r.pulls, fetch)
			r.imageIDs = append(r.imageIDs, imageID)
			return &store.ImageNode{State: store.ImageStatePulling, TransferID: "pull-transfer"}, nil
		}},
		PeerSync: PeerSyncHooks{
			SyncImageWithRate: func(_ context.Context, imageID uint, nodeUUID, sourceNodeUUID string, rate int64) (*store.ImageNode, error) {
				r.peers = append(r.peers, peerCall{imageID, nodeUUID, sourceNodeUUID, rate})
				return &store.ImageNode{State: store.ImageStateSyncing, TransferID: "peer-transfer"}, nil
			},
			RetryImage: func(_ context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error) {
				r.retries = append(r.retries, nodeUUID)
				return &store.ImageNode{State: store.ImageStateSyncing, TransferID: "retry-transfer"}, nil
			},
		},
	}
}

func syncRoute(image *store.Image, uuid string) map[string]string {
	return map[string]string{"id": fmt.Sprint(image.ID), "uuid": uuid}
}

// A sync with no source, or the source "default", is a pull through the node's
// pull source rules: ImageFetch.Pull with an empty Source, never a peer copy.
func TestSyncImageWithoutASourcePullsThroughThePullSourceRules(t *testing.T) {
	for i, body := range []string{``, `{}`, `{"source":""}`, `{"source":"default"}`, `{"source":" Default "}`, `{"source":"default","force":true}`} {
		t.Run(body, func(t *testing.T) {
			st, image := imageWithSources(t, fmt.Sprintf("file:api_sync_pull_%d?mode=memory&cache=shared", i), nil)
			rec := &pullRecorder{}
			h := apiHandlers{d: rec.deps(st)}
			resp := postJSON(t, h.syncImageToNode, "/api/v1/images/1/nodes/node-1/sync", body, syncRoute(image, "node-1"))
			if resp.Code != http.StatusAccepted {
				t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
			}
			if len(rec.pulls) != 1 || len(rec.peers) != 0 || len(rec.retries) != 0 {
				t.Fatalf("pulls=%+v peers=%+v retries=%v, want exactly one pull", rec.pulls, rec.peers, rec.retries)
			}
			if rec.pulls[0].Source != "" || rec.pulls[0].IsURLFetch() || rec.pulls[0].ArchiveURL != "" || rec.imageIDs[0] != image.ID {
				t.Fatalf("pull = %+v image=%d, want an unsourced pull of image %d", rec.pulls[0], rec.imageIDs[0], image.ID)
			}
			if wantForce := strings.Contains(body, "force"); rec.pulls[0].Force != wantForce {
				t.Fatalf("force = %v for body %q", rec.pulls[0].Force, body)
			}
			if got := strings.TrimSpace(resp.Body.String()); got != `{"state":"pulling","transfer_id":"pull-transfer"}` {
				t.Fatalf("wire = %s", got)
			}
		})
	}
}

// A registry or mirror can no longer be named for a pull; the pull settings
// decide. The answer says which input was refused and where the setting lives.
func TestSyncImageRefusesANamedRegistrySource(t *testing.T) {
	st, image := imageWithSources(t, "file:api_sync_removed_source?mode=memory&cache=shared", nil)
	for _, source := range []string{"quay", "huaweicloud", "direct", "offline", "docker.io", "registry.example.com:5000"} {
		for _, action := range []string{"sync", "retry"} {
			name := action
			rec := &pullRecorder{}
			h := apiHandlers{d: rec.deps(st)}
			route := h.syncImageToNode
			if action == "retry" {
				route = h.retryImageToNode
			}
			resp := postJSON(t, route, "/api/v1/images/1/nodes/node-1/sync", fmt.Sprintf(`{"source":%q}`, source), syncRoute(image, "node-1"))
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("%s %q: status = %d body = %s", name, source, resp.Code, resp.Body.String())
			}
			var got struct {
				Code   string         `json:"code"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Code != "IMAGE_SOURCE_REMOVED" || got.Params["source"] != source || got.Params["route"] != "/api/v1/pull/source-rules" || len(got.Params) != 2 {
				t.Fatalf("%s %q: code=%q params=%v", name, source, got.Code, got.Params)
			}
			if len(rec.pulls)+len(rec.peers)+len(rec.retries) != 0 {
				t.Fatalf("%s %q reached a node: %+v %+v", name, source, rec.pulls, rec.peers)
			}
		}
	}
}

// A node the source rules leave nothing to pull from is refused with a stable
// code naming the node and where the rules live, for a sync and a retry alike.
func TestSyncImageForbiddenBySourceRulesNamesTheNode(t *testing.T) {
	st, image := imageWithSources(t, "file:api_sync_forbidden?mode=memory&cache=shared", nil)
	node, err := st.RegisterNode("forbidden-node", store.HashToken("token"), "gpu-a-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "registry.example"}); err != nil {
		t.Fatal(err)
	}
	// The error the hub's pull hook passes on is planning's own.
	_, planErr := planning.PullCandidatesFor(st, nil, node, image.ImageRef, st.ImageCopies(image.ImageRef, nil))
	if !errors.Is(planErr, pullref.ErrNoAllowedSource) {
		t.Fatalf("a rule with no endpoint that forbids upstream must leave nothing to pull: %v", planErr)
	}
	for _, action := range []string{"sync", "retry"} {
		rec := &pullRecorder{}
		deps := rec.deps(st)
		deps.ImageFetch.Pull = func(context.Context, uint, string, imagecatalog.FetchRequest) (*store.ImageNode, error) {
			return nil, planErr
		}
		h := apiHandlers{d: deps}
		route := h.syncImageToNode
		if action == "retry" {
			route = h.retryImageToNode
		}
		resp := postJSON(t, route, "/api/v1/images/1/nodes/forbidden-node/sync", `{}`, syncRoute(image, node.UUID))
		var got struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if resp.Code != http.StatusConflict || got.Code != "IMAGE_SOURCE_FORBIDDEN" ||
			!reflect.DeepEqual(got.Params, map[string]any{"node": "gpu-a-01", "route": "/api/v1/pull/source-rules"}) {
			t.Fatalf("%s: status=%d body=%s", action, resp.Code, resp.Body.String())
		}
	}
}

// Naming a source node is a copy from that node, with the rate cap carried.
func TestSyncImageFromASourceNodeCopiesFromThatNode(t *testing.T) {
	st, image := imageWithSources(t, "file:api_sync_peer_copy?mode=memory&cache=shared", nil)
	rec := &pullRecorder{}
	h := apiHandlers{d: rec.deps(st)}
	resp := postJSON(t, h.syncImageToNode, "/api/v1/images/1/nodes/node-target/sync", `{"source_node":" node-src ","rate_limit_kib_s":2048}`, syncRoute(image, "node-target"))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
	}
	if len(rec.pulls) != 0 || !reflect.DeepEqual(rec.peers, []peerCall{{image.ID, "node-target", "node-src", 2048}}) {
		t.Fatalf("pulls=%+v peers=%+v", rec.pulls, rec.peers)
	}
	if got := strings.TrimSpace(resp.Body.String()); got != `{"state":"syncing","transfer_id":"peer-transfer"}` {
		t.Fatalf("wire = %s", got)
	}
	// A negative cap is refused before any node is asked.
	resp = postJSON(t, h.syncImageToNode, "/api/v1/images/1/nodes/node-target/sync", `{"source_node":"node-src","rate_limit_kib_s":-5}`, syncRoute(image, "node-target"))
	if resp.Code != http.StatusBadRequest || len(rec.peers) != 1 {
		t.Fatalf("negative rate: status = %d peers=%+v body=%s", resp.Code, rec.peers, resp.Body.String())
	}
	// Without the hook the copy is unavailable, not silently a pull.
	bare := apiHandlers{d: Deps{Store: st}}
	resp = postJSON(t, bare.syncImageToNode, "/api/v1/images/1/nodes/node-target/sync", `{"source_node":"node-src"}`, syncRoute(image, "node-target"))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured peer sync: status = %d body=%s", resp.Code, resp.Body.String())
	}
}

// Retry with nothing named repeats what failed: a peer copy that failed is
// retried as one (keeping its recorded source), anything else pulls again
// with Force so a stale "failed" row cannot short-circuit it.
func TestRetryImageRepeatsWhatFailed(t *testing.T) {
	st, image := imageWithSources(t, "file:api_retry_image_what_failed?mode=memory&cache=shared", nil)
	peerNode, err := st.RegisterNode("node-peer-failed", store.HashToken("p"), "peer-failed")
	if err != nil {
		t.Fatal(err)
	}
	pullNode, err := st.RegisterNode("node-pull-failed", store.HashToken("q"), "pull-failed")
	if err != nil {
		t.Fatal(err)
	}
	source, err := st.RegisterNode("node-source", store.HashToken("s"), "source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, NodeID: peerNode.ID, State: store.ImageStateFailed, Source: "peer", SourceNodeID: source.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, NodeID: pullNode.ID, State: store.ImageStateFailed, Source: "pull"}); err != nil {
		t.Fatal(err)
	}
	rec := &pullRecorder{}
	h := apiHandlers{d: rec.deps(st)}

	resp := postJSON(t, h.retryImageToNode, "/api/v1/images/1/nodes/x/sync/retry", `{}`, syncRoute(image, peerNode.UUID))
	if resp.Code != http.StatusAccepted || !reflect.DeepEqual(rec.retries, []string{peerNode.UUID}) || len(rec.pulls) != 0 {
		t.Fatalf("failed peer copy: status=%d retries=%v pulls=%+v body=%s", resp.Code, rec.retries, rec.pulls, resp.Body.String())
	}
	resp = postJSON(t, h.retryImageToNode, "/api/v1/images/1/nodes/x/sync/retry", `{}`, syncRoute(image, pullNode.UUID))
	if resp.Code != http.StatusAccepted || len(rec.retries) != 1 || len(rec.pulls) != 1 {
		t.Fatalf("failed pull: status=%d retries=%v pulls=%+v body=%s", resp.Code, rec.retries, rec.pulls, resp.Body.String())
	}
	if p := rec.pulls[0]; p.Source != "" || !p.Force {
		t.Fatalf("retry pull = %+v, want an unsourced forced pull", p)
	}
	// A node with no row at all (never fetched) is also a forced pull.
	resp = postJSON(t, h.retryImageToNode, "/api/v1/images/1/nodes/x/sync/retry", ``, syncRoute(image, "never-seen"))
	if resp.Code != http.StatusAccepted || len(rec.pulls) != 2 || !rec.pulls[1].Force {
		t.Fatalf("no row: status=%d pulls=%+v", resp.Code, rec.pulls)
	}
}

func TestImageDTOIncludesAbsentOnlineNodeAction(t *testing.T) {
	st, err := store.Open("file:api_image_dto_absent_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, _ := st.UpsertImage(&store.Image{ImageRef: "example/vllm:2", Engine: "vllm", Enabled: true})
	if _, err := st.RegisterNode("node-1", store.HashToken("t"), "one"); err != nil {
		t.Fatal(err)
	}
	got := (&apiHandlers{d: Deps{Store: st}}).imageDTO(image, true)
	if len(got.Nodes) != 1 || got.Nodes[0].NodeUUID != "node-1" || got.Nodes[0].State != store.ImageStateAbsent {
		t.Fatalf("image nodes = %+v", got.Nodes)
	}
}

func TestRetryImageEndpointReturnsAccepted(t *testing.T) {
	st, image := imageWithSources(t, "file:api_retry_image_accepted?mode=memory&cache=shared", nil)
	node, err := st.RegisterNode("node-1", store.HashToken("t"), "one")
	if err != nil {
		t.Fatal(err)
	}
	source, err := st.RegisterNode("node-0", store.HashToken("s"), "zero")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, NodeID: node.ID, State: store.ImageStateFailed, Source: "peer", SourceNodeID: source.ID}); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, PeerSync: PeerSyncHooks{RetryImage: func(ctx context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error) {
		return &store.ImageNode{State: store.ImageStateSyncing, TransferID: "retry-transfer"}, nil
	}}}}
	rec := postJSON(t, h.retryImageToNode, "/api/v1/images/1/nodes/node-1/sync/retry", "{}", syncRoute(image, "node-1"))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "retry-transfer") {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
}

// Images live in each node's container runtime, not in a model storage, so a
// read-only (disabled) storage never stops an image transfer.
func TestImageSyncIgnoresDisabledModelStorages(t *testing.T) {
	st, err := store.Open("file:api_sync_image_storage_disabled_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListStorages()
	if err != nil || len(rows) != 1 {
		t.Fatalf("storages = %+v err=%v", rows, err)
	}
	disabled := false
	if _, err := st.UpdateStorage(rows[0].ID, store.StoragePatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := apiHandlers{d: Deps{
		Store: st,
		ImageFetch: ImageFetchHooks{Pull: func(context.Context, uint, string, imagecatalog.FetchRequest) (*store.ImageNode, error) {
			calls++
			return &store.ImageNode{State: store.ImageStateSyncing}, nil
		}},
	}}
	for _, path := range []string{"/api/v1/images/1/nodes/node-1/sync", "/api/v1/images/1/nodes/node-1/sync/retry"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": "1", "uuid": "node-1"}))
		rec := httptest.NewRecorder()
		if strings.HasSuffix(path, "/retry") {
			h.retryImageToNode(rec, req)
		} else {
			h.syncImageToNode(rec, req)
		}
		if strings.Contains(rec.Body.String(), ErrorLocalStorageDisabled) || strings.Contains(rec.Body.String(), ErrorStorageDisabled) {
			t.Fatalf("%s refused for a disabled model storage: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("image transfer callbacks = %d, want 2", calls)
	}
}

func TestWriteErrSerializesProductError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusConflict, messages.NewProductError(messages.ErrorAgentUpgradeRequired, "Upgrade the agent.", map[string]any{"minimum_version": "v2"}))
	var response struct {
		Error  string         `json:"error"`
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict || response.Error != "Upgrade the agent." || response.Code != messages.ErrorAgentUpgradeRequired || response.Params["minimum_version"] != "v2" {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
}

// An image's node rows name their node, its state and the peer a sync reads
// from, so the image page needs no node list of its own.
func TestImageDTONamesEachNodeAndItsPeerSource(t *testing.T) {
	st, err := store.Open("file:api_image_dto_names_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, _ := st.UpsertImage(&store.Image{ImageRef: "example/vllm:3", Engine: "vllm", Enabled: true})
	source, err := st.RegisterNode("node-src", store.HashToken("src"), "src-host")
	if err != nil {
		t.Fatal(err)
	}
	target, err := st.RegisterNode("node-dst", store.HashToken("dst"), "dst-host")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(source.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, NodeID: target.ID, State: store.ImageStateSyncing, SourceNodeID: source.ID}); err != nil {
		t.Fatal(err)
	}
	got := (&apiHandlers{d: Deps{Store: st}}).imageDTO(image, true)
	raw, err := json.Marshal(got.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	byNode := map[string]map[string]any{}
	for _, row := range rows {
		byNode[row["node_uuid"].(string)] = row
	}
	if src := byNode["node-src"]; src["node_name"] != "src-host" || src["node_state"] != store.NodeStateOnline || src["state"] != store.ImageStateAbsent {
		t.Errorf("source row = %v", src)
	}
	dst := byNode["node-dst"]
	if dst["node_name"] != "dst-host" || dst["node_state"] == "" || dst["source_node_uuid"] != "node-src" || dst["source_node_name"] != "src-host" {
		t.Errorf("target row = %v, want it to name itself and its peer source", dst)
	}
}
