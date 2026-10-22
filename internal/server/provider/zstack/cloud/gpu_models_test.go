package cloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestListInstancesPreservesCloudGPUModels(t *testing.T) {
	for _, tc := range []struct {
		name, inventory, want, wantTarget string
	}{
		{"reported model", `[{"uuid":"gpu-1","hostUuid":"host-1","gpuType":" NVIDIA A100 ","device":"PCI device"}]`, "NVIDIA A100", "NVIDIA A100"},
		{"PCI device fallback", `[{"uuid":"gpu-1","hostUuid":"host-1","gpuType":" ","device":"NVIDIA L40"}]`, "NVIDIA L40", "NVIDIA L40"},
		{"mixed cards", `[{"uuid":"gpu-1","hostUuid":"host-1","gpuType":"NVIDIA L40"},{"uuid":"gpu-2","hostUuid":"host-1","gpuType":"NVIDIA A100"},{"uuid":"gpu-3","hostUuid":"host-1","gpuType":"NVIDIA L40"}]`, "NVIDIA A100, NVIDIA L40", "NVIDIA L40"},
		{"missing model", `[{"uuid":"gpu-1","hostUuid":"host-1"}]`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var inventories []json.RawMessage
			if err := json.Unmarshal([]byte(tc.inventory), &inventories); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !validTestAccessKey(r) {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/hosts":
					fmt.Fprint(w, `{"inventories":[{"uuid":"host-1","hypervisorType":"KVM"}]}`)
				case "/v1/gpu-device/gpu-devices":
					if query := r.URL.Query().Get("q"); query != "" {
						for _, raw := range inventories {
							var identity struct {
								UUID string `json:"uuid"`
							}
							if err := json.Unmarshal(raw, &identity); err != nil {
								t.Error(err)
								return
							}
							if query == "uuid="+identity.UUID {
								fmt.Fprintf(w, `{"inventories":[%s]}`, raw)
								return
							}
						}
						fmt.Fprint(w, `{"inventories":[]}`)
						return
					}
					fmt.Fprintf(w, `{"inventories":%s}`, tc.inventory)
				case "/v1/resource-configurations/host-1/kvm/reservedCpu", "/v1/resource-configurations/host-1/kvm/reservedMemory":
					fmt.Fprint(w, `{"value":"0"}`)
				default:
					t.Errorf("unexpected Cloud request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			creds := provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}
			instances, err := New().ListInstances(t.Context(), creds)
			if err != nil || len(instances) != 1 || instances[0].GPUModel != tc.want || instances[0].GPUCount != len(inventories) {
				t.Fatalf("Cloud host model summary: instances=%+v err=%v, want %q", instances, err, tc.want)
			}
			resource, err := New().QueryHostResources(t.Context(), creds, "host-1", []string{"gpu-1"})
			if err != nil || len(resource.GPUs) != 1 {
				t.Fatalf("targeted discovery = %+v, err=%v", resource, err)
			}
			if resource.GPUs[0].Model != tc.wantTarget {
				t.Fatalf("targeted discovery lost the GPU model: %+v", resource)
			}
		})
	}
}
