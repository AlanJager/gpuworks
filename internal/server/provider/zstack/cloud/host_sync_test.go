package cloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCloudSyncQueriesKVMHostsAndReconcilesInventory(t *testing.T) {
	var failGPUQuery, removeKVMHosts atomic.Bool
	var filteredQueries atomic.Int32
	hosts := []map[string]string{
		{"uuid": "host-empty", "hypervisorType": "KVM"},
		{"uuid": "host-kvm", "hypervisorType": "KVM", "managementIp": "192.0.2.10", "status": "Connected"},
		{"uuid": "host-native", "hypervisorType": "Native", "managementIp": "192.0.2.11"},
		{"uuid": "host-offline", "hypervisorType": "KVM", "status": "Disconnected"},
		{"uuid": "host-unknown", "hypervisorType": ""},
		{"uuid": "host-vmware", "hypervisorType": "ESX"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !validTestAccessKey(r) {
			t.Errorf("unexpected Cloud request: %s %s", r.Method, r.URL)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hosts":
			query := r.URL.Query()
			if query.Get("start") != "0" || query.Get("limit") != "100" || query.Get("sort") != "uuid" {
				t.Errorf("invalid pagination: %s", r.URL)
				http.Error(w, "invalid pagination", http.StatusBadRequest)
				return
			}
			filtered := make([]map[string]string, 0)
			for _, host := range hosts {
				if removeKVMHosts.Load() && host["hypervisorType"] == "KVM" {
					continue
				}
				matches := true
				for _, condition := range query["q"] {
					key, value, ok := strings.Cut(condition, "=")
					if !ok || key != "hypervisorType" {
						http.Error(w, "unsupported condition", http.StatusBadRequest)
						return
					}
					matches = matches && host[key] == value
				}
				if matches {
					filtered = append(filtered, host)
				}
			}
			if slices.Equal(query["q"], []string{"hypervisorType=KVM"}) {
				filteredQueries.Add(1)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"inventories": filtered})
		case "/v1/gpu-device/gpu-devices":
			if failGPUQuery.Load() {
				http.Error(w, "GPU inventory unavailable", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"inventories":[
				{"uuid":"gpu-used","hostUuid":"host-kvm","state":"Enabled","allocateStatus":"Allocated"},
				{"uuid":"gpu-disabled","hostUuid":"host-kvm","state":"Disabled","allocateStatus":"Unallocated"},
				{"uuid":"gpu-native","hostUuid":"host-native"},
				{"uuid":"gpu-offline","hostUuid":"host-offline"},
				{"uuid":"gpu-unknown","hostUuid":"host-unknown"},
				{"uuid":"gpu-vmware","hostUuid":"host-vmware"}
			]}`))
		default:
			if strings.HasPrefix(r.URL.Path, "/v1/resource-configurations/host-") &&
				(strings.HasSuffix(r.URL.Path, "/kvm/reservedMemory") || strings.HasSuffix(r.URL.Path, "/kvm/reservedCpu")) {
				_, _ = w.Write([]byte(`{"value":"0"}`))
				return
			}
			t.Errorf("unexpected Cloud query: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("cloud-host-sync-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	svc.SetDriver(New())
	p, err := svc.Add(t.Context(), "zstack", "Cloud", provider.Credentials{
		Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.CreateNodePending("KVM node")
	if err != nil {
		t.Fatal(err)
	}
	legacy := store.ProviderInstance{ProviderID: p.ID, ExternalID: "host-native"}
	kept := store.ProviderInstance{ProviderID: p.ID, ExternalID: "host-kvm", NodeID: node.ID}
	for _, row := range []*store.ProviderInstance{&legacy, &kept} {
		if err := st.UpsertProviderInstance(row); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		before := filteredQueries.Load()
		instances, err := svc.Sync(t.Context(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(instances))
		for _, instance := range instances {
			ids = append(ids, instance.ExternalID)
		}
		if !slices.Equal(ids, []string{"host-empty", "host-kvm", "host-offline"}) {
			t.Fatalf("synced hosts = %v, want all KVM hosts, including disconnected hosts", ids)
		}
		if filteredQueries.Load() != before+1 {
			t.Fatal("host filtering must happen in the Cloud query")
		}
		rows, err := st.ProviderInstances(p.ID)
		if err != nil || len(rows) != 3 {
			t.Fatalf("stored inventory = %+v, err=%v, want three KVM hosts without the old Native row", rows, err)
		}
		byID := make(map[string]store.ProviderInstance, len(rows))
		for _, row := range rows {
			byID[row.ExternalID] = row
		}
		keptRow := byID["host-kvm"]
		if keptRow.ID != kept.ID || keptRow.NodeID != node.ID || keptRow.GPUCount != 2 {
			t.Fatalf("sync lost KVM identity, node link or allocated/disabled GPUs: %+v", keptRow)
		}
		if byID["host-empty"].ID == 0 || byID["host-empty"].GPUCount != 0 || byID["host-offline"].ID == 0 {
			t.Fatalf("sync lost the empty or offline KVM host: %+v", rows)
		}
	}
	// A failed discovery must not commit its partially gathered host list.
	legacy.ID = 0
	if err := st.UpsertProviderInstance(&legacy); err != nil {
		t.Fatal(err)
	}
	before, err := st.ProviderInstances(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	failGPUQuery.Store(true)
	if _, err := svc.Sync(t.Context(), p.ID); err == nil {
		t.Fatal("sync succeeded despite a failed GPU inventory query")
	}
	after, err := st.ProviderInstances(p.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed discovery changed inventory: before=%+v after=%+v err=%v", before, after, err)
	}
	// An empty KVM result is a successful listing even when Native hosts remain.
	failGPUQuery.Store(false)
	removeKVMHosts.Store(true)
	instances, err := svc.Sync(t.Context(), p.ID)
	if err != nil || len(instances) != 0 {
		t.Fatalf("empty KVM listing: instances=%+v err=%v", instances, err)
	}
	rows, err := st.ProviderInstances(p.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty KVM listing left stale inventory: rows=%+v err=%v", rows, err)
	}
}
