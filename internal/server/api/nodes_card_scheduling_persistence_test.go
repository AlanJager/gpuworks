package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCardSchedulingEnablePersistsAcrossReportsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cards.db")
	st, err := store.Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	admin, ok := st.VerifyUser("admin", "gpuworks-test-password-2026")
	if !ok {
		t.Fatal("admin missing")
	}
	signer, err := auth.NewTokenSigner("card-scheduling-public-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: admin.ID, Username: admin.Username, Role: admin.Role})
	if err != nil {
		t.Fatal(err)
	}
	handler := Router(Deps{Store: st, AuthSigner: signer})
	node, err := st.RegisterNode("persistent-card", store.HashToken("node-test-token"), "persistent-card")
	if err != nil {
		t.Fatal(err)
	}
	report := func(fault, foreign bool, ownership string) {
		t.Helper()
		gpu := proto.GPUInfo{Index: 0, UUID: "GPU-persistent", PCIeBDF: "0000:40:00.0", Vendor: "nvidia", MemoryTotalBytes: 24 << 30, Healthy: true, Ownership: ownership}
		if fault {
			gpu.HealthCode, gpu.HealthStatus, gpu.Healthy = accelerator.HealthRuntimeUnusable, accelerator.HealthLevel(accelerator.HealthRuntimeUnusable), false
		}
		if foreign {
			gpu.ForeignProcesses = []proto.CardProcess{{PID: 42, Name: "python3", MemoryBytes: 1 << 30}}
		}
		probeOK := true
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUProbeOK: &probeOK, GPUs: []proto.GPUInfo{gpu}, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
			t.Fatal(err)
		}
	}
	call := func(method, suffix, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1/nodes/"+node.UUID+suffix, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	card := func() map[string]json.RawMessage {
		t.Helper()
		rec := call(http.MethodGet, "", "")
		var out struct {
			GPUs []map[string]json.RawMessage `json:"gpus"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.GPUs) != 1 {
			t.Fatalf("card = %d %s", rec.Code, rec.Body)
		}
		return out.GPUs[0]
	}
	set := func(value string) {
		t.Helper()
		rec := call(http.MethodPut, "/gpus/scheduling", `{"pcie_bdf":"0000:40:00.0","scheduling":"`+value+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("set %s = %d %s", value, rec.Code, rec.Body)
		}
	}

	// Automatic facts still keep a faulty, externally held card out of work.
	report(true, true, "")
	if got := card(); string(got["schedulable"]) != "false" {
		t.Fatalf("unmodified bad card was schedulable: %s", got)
	}
	set("schedulable")
	if got := card(); string(got["schedulable"]) != "true" || string(got["scheduling_allowed"]) != "true" {
		t.Fatalf("enable did not take effect: %s", got)
	}

	// Age the existing operator decision rather than sleeping a minute. This
	// is an independent database writer; all actions/readback use real HTTP.
	external, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := external.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	aged := external.Table("gpus").Where("node_id = ?", node.ID).Update("scheduling_allowed_at", time.Now().Add(-24*time.Hour))
	if aged.Error != nil || aged.RowsAffected != 1 {
		t.Fatalf("age decision = %d, %v", aged.RowsAffected, aged.Error)
	}
	for i := 0; i < 3; i++ {
		report(true, true, "")
	}
	got := card()
	if string(got["schedulable"]) != "true" || string(got["scheduling_allowed"]) != "true" {
		t.Fatalf("an old explicit enable was revoked by a report: %s", got)
	}
	if string(got["health_code"]) != `"`+accelerator.HealthRuntimeUnusable+`"` || string(got["health_status"]) != `"Error"` {
		t.Fatalf("enabling hid the hardware failure: %s", got)
	}
	var foreign []struct {
		PID         int    `json:"pid"`
		Name        string `json:"name"`
		MemoryBytes int64  `json:"memory_bytes"`
	}
	if err := json.Unmarshal(got["foreign_processes"], &foreign); err != nil || len(foreign) != 1 || foreign[0].PID != 42 || foreign[0].Name != "python3" || foreign[0].MemoryBytes != 1<<30 {
		t.Fatalf("enabling hid outside-use evidence: %s, %v", got["foreign_processes"], err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	handler = Router(Deps{Store: st, AuthSigner: signer})
	if got := card(); string(got["schedulable"]) != "true" {
		t.Fatalf("restart lost explicit scheduling decision: %s", got)
	}

	set("stopped")
	report(false, false, "")
	if got := card(); string(got["schedulable"]) != "false" || string(got["scheduling_stopped"]) != "true" {
		t.Fatalf("a healthy report overrode manual disable: %s", got)
	}
	// Enabling a healthy card is also a persistent decision, not an accidental
	// return to automatic enforcement when a later report changes its facts.
	set("schedulable")
	report(true, true, "")
	if got := card(); string(got["schedulable"]) != "true" {
		t.Fatalf("enable on a healthy card was not persistent: %s", got)
	}
	// An operator cannot consent to use a card no longer owned by this node.
	report(false, false, "passthrough")
	if got := card(); string(got["schedulable"]) != "false" {
		t.Fatalf("explicit enable bypassed card ownership: %s", got)
	}
	if rec := call(http.MethodPut, "/gpus/scheduling", `{"pcie_bdf":"0000:40:00.0","scheduling":"schedulable"}`); rec.Code != http.StatusConflict {
		t.Fatalf("enable passthrough = %d %s", rec.Code, rec.Body)
	}
}
