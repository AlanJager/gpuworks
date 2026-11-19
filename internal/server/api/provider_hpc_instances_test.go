package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// hpcNodeDriver is a vendor account holding compute nodes of HPC clusters
// (E-HPC) beside a plain instance.
type hpcNodeDriver struct{ clusterNodeDriver }

func (d *hpcNodeDriver) Name() string { return "api-hpc-node-provider" }

// An HPC cluster's compute nodes are the cluster's, as a managed Kubernetes
// cluster's nodes are: the account's instance list, its instance count and a
// sync's count leave them out (the cluster is listed by /hpc-clusters), while
// powering, releasing or installing on one is still refused with
// PROVIDER_INSTANCE_IN_HPC_CLUSTER before the vendor hears of any.
func TestProviderHPCComputeNodesAreTheClustersNotTheAccountsInstances(t *testing.T) {
	slurm := inventoryInstance("i-ehpc", "ehpc-sz-1_gpu000", "")
	slurm.HPC = &provider.HPCCluster{ID: "ehpc-sz-1", Name: "slurm-lab", Scheduler: "slurm", SchedulerVersion: "22.05.8"}
	pbs := inventoryInstance("i-pbs", "ehpc-sz-2_comp000", "")
	pbs.HPC = &provider.HPCCluster{ID: "ehpc-sz-2", Name: "pbs-lab", Scheduler: "pbs", SchedulerVersion: "19.1.1"}
	drv := &hpcNodeDriver{clusterNodeDriver{apiInventoryDriver: apiInventoryDriver{instances: map[string]provider.Instance{
		"i-ehpc": slurm, "i-pbs": pbs, "i-plain": inventoryInstance("i-plain", "plain", "203.0.113.21"),
	}}}}
	provider.Register(drv)
	st, err := store.Open("file:api_provider_hpc_node?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("api-provider-hpc-node")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), drv.Name(), "cloud", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("provider-hpc-node-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, Providers: svc, AuthSigner: signer})
	base := fmt.Sprintf("/api/v1/providers/%d", p.ID)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(http.MethodPost, base+"/sync", ""); rec.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", rec.Code, rec.Body)
	}
	body := call(http.MethodGet, base+"/instances", "").Body.String()
	if !strings.Contains(body, `"external_id":"i-plain"`) || !strings.Contains(body, `"node_id":0,"actions":["power_off","install","delete"]}`) || strings.Contains(body, "i-ehpc") || strings.Contains(body, "i-pbs") || strings.Contains(body, "hpc_cluster") {
		t.Fatalf("the account's instances = %s; want the plain instance alone", body)
	}
	if body := call(http.MethodPost, base+"/sync", "").Body.String(); !strings.Contains(body, `"count":1`) {
		t.Fatalf("sync = %s; want a count of the account's own instance", body)
	}
	if body := call(http.MethodGet, "/api/v1/providers", "").Body.String(); !strings.Contains(body, `"instance_count":1`) {
		t.Fatalf("providers = %s; want the cluster's nodes left out of instance_count", body)
	}

	for _, req := range []struct{ method, path, body string }{
		{http.MethodPost, base + "/instances/i-ehpc/power", `{"action":"off"}`},
		{http.MethodPost, base + "/instances/i-ehpc/power", `{"action":"on"}`},
		{http.MethodDelete, base + "/instances/i-ehpc", ""},
		{http.MethodPost, base + "/instances/i-ehpc/resize", `{"gpu_count":1}`},
	} {
		rec := call(req.method, req.path, req.body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"PROVIDER_INSTANCE_IN_HPC_CLUSTER"`) || !strings.Contains(rec.Body.String(), `"params":{"cluster":"ehpc-sz-1"}`) {
			t.Fatalf("%s %s = %d %s", req.method, req.path, rec.Code, rec.Body)
		}
	}
	for _, ext := range []struct{ id, cluster string }{{"i-ehpc", "ehpc-sz-1"}, {"i-pbs", "ehpc-sz-2"}} {
		rec := call(http.MethodPost, base+"/install", `{"external_id":"`+ext.id+`","server_url":"https://gw.example"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"PROVIDER_INSTANCE_IN_HPC_CLUSTER"`) || !strings.Contains(rec.Body.String(), `"params":{"cluster":"`+ext.cluster+`"}`) {
			t.Fatalf("install on %s = %d %s", ext.id, rec.Code, rec.Body)
		}
	}
	if len(drv.calls) != 0 {
		t.Fatalf("a refused operation reached the vendor: %v", drv.calls)
	}
}
