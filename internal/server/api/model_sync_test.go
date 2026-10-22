package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelSyncAndCancelEndpoints(t *testing.T) {
	var syncModelID, cancelModelID uint
	var syncNode, cancelNode string
	h := apiHandlers{d: Deps{
		SyncModel: func(_ context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error) {
			syncModelID, syncNode = modelID, nodeUUID
			return &store.ModelFile{State: proto.MFSyncing, TransferID: "transfer-1"}, nil
		},
		CancelModelSync: func(_ context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error) {
			cancelModelID, cancelNode = modelID, nodeUUID
			return &store.ModelFile{State: proto.MFDownloadRequired, TransferID: "transfer-1", Message: "canceled"}, nil
		},
	}}

	syncReq := httptest.NewRequest(http.MethodPost, "/api/v1/models/7/nodes/node-1/sync", nil)
	syncReq = syncReq.WithContext(chiRouteContext(syncReq.Context(), map[string]string{"id": "7", "uuid": "node-1"}))
	syncRec := httptest.NewRecorder()
	h.syncModelToNode(syncRec, syncReq)
	if syncRec.Code != http.StatusAccepted || syncModelID != 7 || syncNode != "node-1" || !strings.Contains(syncRec.Body.String(), "transfer-1") {
		t.Fatalf("sync response/call = %d %s id=%d node=%q", syncRec.Code, syncRec.Body.String(), syncModelID, syncNode)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/v1/models/7/nodes/node-1/sync/cancel", nil)
	cancelReq = cancelReq.WithContext(chiRouteContext(cancelReq.Context(), map[string]string{"id": "7", "uuid": "node-1"}))
	cancelRec := httptest.NewRecorder()
	h.cancelModelSync(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK || cancelModelID != 7 || cancelNode != "node-1" || !strings.Contains(cancelRec.Body.String(), "download_required") {
		t.Fatalf("cancel response/call = %d %s id=%d node=%q", cancelRec.Code, cancelRec.Body.String(), cancelModelID, cancelNode)
	}
}

func TestModelDTOIncludesPeerSyncDetails(t *testing.T) {
	st, err := store.Open("file:api_model_sync_dto?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "test/model", Revision: "main", Name: "model"})
	source, _ := st.RegisterNode("source-node", store.HashToken("source"), "source")
	target, _ := st.RegisterNode("target-node", store.HashToken("target"), "target")
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: target.ID, Path: "model", State: proto.MFSyncing,
		SizeBytes: 1000, Downloaded: 250, TransferredBytes: 300, RateBPS: 125, Acquisition: proto.ModelAcquirePeer,
		TransferID: "transfer-1", TransferSourceNodeID: source.ID, Message: "copying",
	}); err != nil {
		t.Fatal(err)
	}
	d := (&apiHandlers{d: Deps{Store: st}}).modelDTO(model, true)
	if len(d.Nodes) != 1 {
		t.Fatalf("model presence = %+v", d.Nodes)
	}
	p := d.Nodes[0]
	if p.RateBps != 125 || p.TransferredBytes != 300 || p.Acquisition != proto.ModelAcquirePeer || p.TransferID != "transfer-1" ||
		p.SourceNodeUUID != source.UUID || p.Message != "copying" || p.Progress != 0.25 {
		t.Fatalf("peer sync DTO = %+v", p)
	}
}

func TestListModelsIncludesNodePresence(t *testing.T) {
	st, err := store.Open("file:api_models_presence_list?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "test/list", Revision: "main", Name: "list"})
	node, _ := st.RegisterNode("list-node", store.HashToken("token"), "list-node")
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "list", State: proto.MFPresent,
		SizeBytes: 10, Downloaded: 10, Acquisition: proto.ModelAcquirePeer,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var models []dto.Model
	if err := unmarshalList(rec.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || len(models[0].Nodes) != 1 || models[0].Nodes[0].NodeUUID != node.UUID {
		t.Fatalf("models response = %+v", models)
	}
}

func TestModelSyncOptionalRateLimit(t *testing.T) {
	for _, tc := range []struct {
		body         string
		rate, status int
	}{
		{"{}", -1, 202}, {`{"source_node":"src"}`, -1, 202},
		{`{"source_node":"src","rate_limit_kib_s":0}`, 0, 202},
		{`{"source_node":"src","rate_limit_kib_s":10240}`, 10240, 202},
		{`{"rate_limit_kib_s":-1}`, 0, 400},
	} {
		t.Run(tc.body, func(t *testing.T) {
			called := false
			legacy := func(context.Context, uint, string) (*store.ModelFile, error) {
				t.Fatal("legacy sync called")
				return nil, nil
			}
			h := apiHandlers{d: Deps{SyncModel: legacy, PeerSync: PeerSyncHooks{SyncModelWithOptions: func(_ context.Context, _ uint, _ string, _ string, rate int) (*store.ModelFile, error) {
				called = true
				if rate != tc.rate {
					t.Fatalf("rate=%d want %d", rate, tc.rate)
				}
				return &store.ModelFile{State: proto.MFSyncing}, nil
			}}}}
			rec := postJSON(t, h.syncModelToNode, "/api/v1/models/7/nodes/dst/sync", tc.body, map[string]string{"id": "7", "uuid": "dst"})
			if rec.Code != tc.status || called != (tc.status == 202) {
				t.Fatalf("status=%d called=%v body=%s", rec.Code, called, rec.Body)
			}
		})
	}
}
