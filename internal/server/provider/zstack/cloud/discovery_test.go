package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestDiscoverResourcesTreatsMissingCPUReservationAsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validTestAccessKey(r) {
			t.Fatal("missing Cloud authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hosts":
			_, _ = w.Write([]byte(`{"inventories":[{"uuid":"host-1","name":"GPU host","managementIp":"192.0.2.10","hypervisorType":"KVM","state":"Enabled","status":"Connected","totalCpuCapacity":32,"availableCpuCapacity":24,"totalMemoryCapacity":68719476736,"availableMemoryCapacity":34359738368}]}`))
		case "/v1/gpu-device/gpu-devices":
			_, _ = w.Write([]byte(`{"inventories":[]}`))
		case "/v1/resource-configurations/host-1/kvm/reservedMemory":
			_, _ = w.Write([]byte(`{"value":"1G"}`))
		case "/v1/resource-configurations/host-1/kvm/reservedCpu":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"success":"false","diagnostic":{"code":"20007","message":"global configuration [category:kvm, name:kvm.reservedCpu] cannot bind to resource"}}`))
		case "/v1/global-configurations":
			_, _ = w.Write([]byte("{\"inventories\":[]}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resources, err := New().DiscoverResources(context.Background(), provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 {
		t.Fatalf("resources = %+v", resources)
	}
	if resources[0].CPU.Reserved == nil || *resources[0].CPU.Reserved != 0 {
		t.Fatalf("CPU reservation = %v, want an explicit zero baseline", resources[0].CPU.Reserved)
	}
	if resources[0].Memory.Reserved == nil || *resources[0].Memory.Reserved != 1<<30 {
		t.Fatalf("memory reservation = %v, want 1 GiB", resources[0].Memory.Reserved)
	}
}

func TestDiscoverResourcesPrefersHostCPUReservationOverGlobal(t *testing.T) {
	globalCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hosts":
			_, _ = w.Write([]byte(`{"inventories":[{"uuid":"host-1","name":"GPU host","hypervisorType":"KVM"}]}`))
		case "/v1/gpu-device/gpu-devices":
			_, _ = w.Write([]byte(`{"inventories":[]}`))
		case "/v1/resource-configurations/host-1/kvm/reservedMemory":
			_, _ = w.Write([]byte(`{"value":"1G"}`))
		case "/v1/resource-configurations/host-1/kvm/reservedCpu":
			_, _ = w.Write([]byte(`{"value":"5"}`))
		case "/v1/global-configurations":
			globalCalls++
			_, _ = w.Write([]byte(`{"inventories":[{"name":"reservedCpu","category":"kvm","value":"3","defaultValue":"0"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resources, err := New().DiscoverResources(context.Background(), provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if got := *resources[0].CPU.Reserved; got != 5 {
		t.Fatalf("CPU reservation = %d, want host override 5", got)
	}
	if globalCalls != 0 {
		t.Fatalf("global config calls = %d, want no fallback call", globalCalls)
	}
}

func TestDiscoverResourcesUsesGlobalCPUReservationFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hosts":
			_, _ = w.Write([]byte(`{"inventories":[{"uuid":"host-1","name":"GPU host","hypervisorType":"KVM"}]}`))
		case "/v1/gpu-device/gpu-devices":
			_, _ = w.Write([]byte(`{"inventories":[]}`))
		case "/v1/resource-configurations/host-1/kvm/reservedMemory":
			_, _ = w.Write([]byte(`{"value":"1G"}`))
		case "/v1/resource-configurations/host-1/kvm/reservedCpu":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"success":"false","diagnostic":{"code":"20007"},"error":{"code":"SYS.1007","globalErrorCode":"ORG_ZSTACK_RESOURCECONFIG_10007"}}`))
		case "/v1/global-configurations":
			_, _ = w.Write([]byte(`{"inventories":[{"name":"reservedCpu","category":"kvm","value":"3","defaultValue":"0"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resources, err := New().DiscoverResources(context.Background(), provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if got := *resources[0].CPU.Reserved; got != 3 {
		t.Fatalf("CPU reservation = %d, want global fallback 3", got)
	}
}
