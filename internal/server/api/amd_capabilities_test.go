package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnginesAMDTargetCapabilities(t *testing.T) {
	h := &apiHandlers{}
	rec := httptest.NewRecorder()
	h.engines(rec, httptest.NewRequest(http.MethodGet, "/api/v1/engines?target_vendor=amd", nil))
	var response struct {
		Items []struct {
			Name         string `json:"name"`
			DefaultImage string `json:"default_image"`
			Sleep        struct {
				Supported bool `json:"supported"`
			} `json:"sleep"`
			MultiNodeSupported *bool             `json:"multi_node_supported"`
			PDModes            []json.RawMessage `json:"pd_modes"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(response.Items) == 0 {
		t.Fatalf("engine response=%d %s", rec.Code, rec.Body.String())
	}
	for _, item := range response.Items {
		if item.DefaultImage != "" {
			t.Fatalf("AMD profile %s names a default image: %+v", item.Name, item)
		}
		// AMD runs multi-node ranks, so the engine profile decides, as on
		// other vendors: no target override.
		if item.MultiNodeSupported != nil {
			t.Fatalf("AMD profile %s overrides multi-node support: %v", item.Name, *item.MultiNodeSupported)
		}
		var modes []string
		for _, raw := range item.PDModes {
			var mode struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &mode); err != nil {
				t.Fatal(err)
			}
			modes = append(modes, mode.Name)
		}
		// vLLM sleeps and runs NIXL PD on ROCm; only modes declared for AMD
		// are listed.
		if item.Name == "vllm" {
			if !item.Sleep.Supported || len(modes) != 1 || modes[0] != "vllm-nixl" {
				t.Fatalf("AMD vLLM profile sleep=%v modes=%v", item.Sleep.Supported, modes)
			}
		} else if len(modes) != 0 {
			t.Fatalf("AMD %s profile lists PD modes not declared for AMD: %v", item.Name, modes)
		}
	}
}

func TestEnginesNonAMDProfilesOmitTargetMultiNodeOverride(t *testing.T) {
	h := &apiHandlers{}
	for _, tc := range []struct {
		name string
		url  string
	}{
		{name: "default", url: "/api/v1/engines"},
		{name: "nvidia", url: "/api/v1/engines?target_vendor=nvidia"},
		{name: "huawei", url: "/api/v1/engines?target_vendor=huawei"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.engines(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
			var response struct {
				Items []map[string]json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || len(response.Items) == 0 {
				t.Fatalf("engine response=%d %s", rec.Code, rec.Body.String())
			}
			for index, item := range response.Items {
				if value, exists := item["multi_node_supported"]; exists {
					t.Errorf("profile %d has target override %s; non-AMD responses must retain engine-profile behavior by omitting it", index, value)
				}
			}
		})
	}
}

func TestEnginesRejectsUnknownTargetVendor(t *testing.T) {
	h := &apiHandlers{}
	rec := httptest.NewRecorder()
	h.engines(rec, httptest.NewRequest(http.MethodGet, "/api/v1/engines?target_vendor=foo", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown target vendor status=%d body=%s", rec.Code, rec.Body.String())
	}
}
