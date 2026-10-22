package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageLegacyTargetsRemainEditableThroughAPI(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("legacy-target", store.HashToken("token"), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := st.CreateStorage(&store.Storage{Name: "legacy-cache", Type: proto.StorageNFSCache, Enabled: true, Export: "nfs:/legacy", CachePath: "/cache"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, last := int64(100), int64(200)
	legacy := []store.StorageNode{
		{StorageID: cache.ID, NodeID: node.ID, Path: "/existing/mount", CacheMaxBytes: &first},
		{StorageID: cache.ID, NodeID: node.ID, LabelValue: "legacy", CachePath: "/effective/cache", CacheMaxBytes: &last, Existing: true, OverrideOnly: true},
	}
	if err := st.DB().Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	signer, admin := roleTestToken(t, st, "admin")
	router := Router(Deps{Store: st, AuthSigner: signer})
	endpoint := fmt.Sprintf("/api/v1/storages/%d", cache.ID)
	request := func(method string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, endpoint, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	get := request(http.MethodGet, nil)
	if get.Code != http.StatusOK {
		t.Fatal(get.Body.String())
	}
	var response dto.Storage
	if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Targets) != 1 {
		t.Fatalf("legacy duplicate targets exposed to the editor: %+v", response.Targets)
	}
	target := response.Targets[0]
	if target.OverrideOnly || target.LabelKey != "" || target.LabelValue != "" || target.Path != "/existing/mount" || target.CachePath != "/effective/cache" || !target.Existing || target.CacheMaxBytes == nil || *target.CacheMaxBytes != last {
		t.Fatalf("canonical response changed membership or effective configuration: %+v", target)
	}
	// A normal scope edit sends the displayed targets plus the new selector.
	targets := append(response.Targets, dto.StorageTarget{LabelKey: "pool", LabelValue: "added"})
	body, err := json.Marshal(dto.StorageRequest{ExpectedUpdatedAt: response.UpdatedAt, Targets: &targets})
	if err != nil {
		t.Fatal(err)
	}
	patch := request(http.MethodPatch, body)
	if patch.Code != http.StatusOK {
		t.Fatalf("scope edit of legacy configuration rejected: %d %s", patch.Code, patch.Body.String())
	}
	eff, applies, err := st.EffectiveStorageForNode(cache, node)
	if err != nil || !applies || eff.Path != target.Path || eff.CachePath != target.CachePath || eff.CacheMaxBytes != last || !eff.Existing {
		t.Fatalf("scope edit changed legacy effective policy: %+v applies=%v err=%v", eff, applies, err)
	}
}
