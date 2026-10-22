package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func pct(v float64) *float64 { return &v }

// gpuListHandler serves GET /gpus from a fixed inventory: the cache is warm,
// so the handler pages exactly these rows.
func gpuListHandler(items []dto.PhysicalGPU) *apiHandlers {
	h := &apiHandlers{}
	overview := dto.GPUOverview{Total: len(items)}
	h.gpuCache = &gpuInventoryCache{physical: dto.GPUListResponse{Items: items, Overview: overview}}
	h.gpuCacheAt = time.Now()
	return h
}

func gpuFleet() []dto.PhysicalGPU {
	var items []dto.PhysicalGPU
	for n, node := range []string{"gpu-b", "gpu-a", "ascend-1"} {
		for i := 0; i < 3; i++ {
			gpu := dto.PhysicalGPU{
				Index: i, UUID: fmt.Sprintf("GPU-%s-%d", node, i), NodeUUID: "uuid-" + node, NodeName: node,
				Name: "NVIDIA H100", Vendor: "nvidia", AllocationState: "available", Occupants: []dto.GPUOccupant{},
			}
			if node == "ascend-1" {
				gpu.Name, gpu.Vendor = "910B4", "ascend"
			}
			if i == 2 {
				gpu.AllocationState = "unusable"
			}
			if n != 2 {
				gpu.Load.UtilizationPct = pct(float64(10*n + i))
			}
			items = append(items, gpu)
		}
	}
	return items
}

func getGPUPage(t *testing.T, h *apiHandlers, query string) (int, dto.GPUPage, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.listGPUs(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpus?"+query, nil))
	var page dto.GPUPage
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, page, rec.Body.String()
}

func gpuKeys(page dto.GPUPage) []string {
	out := make([]string, 0, len(page.Items))
	for _, gpu := range page.Items {
		out = append(out, fmt.Sprintf("%s/%d", gpu.NodeName, gpu.Index))
	}
	return out
}

// Without page parameters GET /gpus stays the full list scripts read.
func TestGPUListWithoutPageIsTheFullInventory(t *testing.T) {
	h := gpuListHandler(gpuFleet())
	rec := httptest.NewRecorder()
	h.listGPUs(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpus", nil))
	var full dto.GPUListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil || len(full.Items) != 9 || strings.Contains(rec.Body.String(), `"page"`) {
		t.Fatalf("full list = %s (err %v)", rec.Body.String(), err)
	}
}

// A page is cut after search, filters and sort; the overview and facets
// always describe the whole fleet.
func TestGPUPageFiltersSortsAndPagesTheWholeFleet(t *testing.T) {
	h := gpuListHandler(gpuFleet())
	code, page, body := getGPUPage(t, h, "page=1&page_size=4&sort=node")
	if code != http.StatusOK {
		t.Fatalf("status %d %s", code, body)
	}
	if page.Total != 9 || page.PageSize != 4 || !reflect.DeepEqual(gpuKeys(page), []string{"ascend-1/0", "ascend-1/1", "ascend-1/2", "gpu-a/0"}) {
		t.Fatalf("page 1 by node = %v (total %d)", gpuKeys(page), page.Total)
	}
	if page.Overview.Total != 9 {
		t.Fatalf("overview = %+v, want the whole fleet", page.Overview)
	}
	wantNodes := []dto.GPUFacetNode{{UUID: "uuid-ascend-1", Name: "ascend-1"}, {UUID: "uuid-gpu-a", Name: "gpu-a"}, {UUID: "uuid-gpu-b", Name: "gpu-b"}}
	if !reflect.DeepEqual(page.Facets.Nodes, wantNodes) || !reflect.DeepEqual(page.Facets.Models, []string{"910B4", "NVIDIA H100"}) || !reflect.DeepEqual(page.Facets.Vendors, []string{"ascend", "nvidia"}) {
		t.Fatalf("facets = %+v", page.Facets)
	}

	_, filtered, _ := getGPUPage(t, h, "page=1&vendor=nvidia&allocation_state=unusable&sort=-node")
	if !reflect.DeepEqual(gpuKeys(filtered), []string{"gpu-b/2", "gpu-a/2"}) || filtered.Total != 2 {
		t.Fatalf("nvidia unusable, node descending = %v", gpuKeys(filtered))
	}
	if !reflect.DeepEqual(filtered.Facets.Nodes, wantNodes) {
		t.Fatal("facets narrowed with the filter; they must offer every value")
	}
	_, nodes, _ := getGPUPage(t, h, "page=1&node=uuid-gpu-a,uuid-ascend-1&allocation_state=unusable")
	if !reflect.DeepEqual(gpuKeys(nodes), []string{"gpu-a/2", "ascend-1/2"}) {
		t.Fatalf("two nodes, unusable = %v", gpuKeys(nodes))
	}
	_, searched, _ := getGPUPage(t, h, "page=1&q=GPU-GPU-B-1")
	if !reflect.DeepEqual(gpuKeys(searched), []string{"gpu-b/1"}) {
		t.Fatalf("q by GPU uuid = %v", gpuKeys(searched))
	}
	// A GPU without a reading sorts before every GPU with one.
	_, byUtil, _ := getGPUPage(t, h, "page=1&page_size=200&sort=utilization")
	if got := gpuKeys(byUtil); !reflect.DeepEqual(got[:4], []string{"ascend-1/0", "ascend-1/1", "ascend-1/2", "gpu-b/0"}) || got[8] != "gpu-a/2" {
		t.Fatalf("by utilization = %v", got)
	}
}

func TestGPUPageRejectsUnknownFiltersAndSorts(t *testing.T) {
	h := gpuListHandler(gpuFleet())
	for _, query := range []string{"page=1&allocation_state=hot", "page=1&allocation_state=free", "page=1&sort=work_state", "page=1&vendor=nvidia,", "page=0"} {
		code, _, body := getGPUPage(t, h, query)
		if code != http.StatusBadRequest || !strings.Contains(body, `"code":"LIST_`) {
			t.Errorf("%s = %d %s, want 400 LIST_*", query, code, body)
		}
	}
}

func TestGPUPageWireJSON(t *testing.T) {
	page := dto.GPUPage{
		ListPage: dto.ListPage[dto.PhysicalGPU]{Items: []dto.PhysicalGPU{}, Page: 2, PageSize: 20, Total: 21},
		Overview: dto.GPUOverview{Total: 21},
		Facets:   dto.GPUFacets{Nodes: []dto.GPUFacetNode{{UUID: "u", Name: "gpu3"}}, Models: []string{"H100"}, Vendors: []string{"nvidia"}},
	}
	got, _ := json.Marshal(page)
	const want = `{"items":[],"page":2,"page_size":20,"total":21,"overview":{"total":21,"health":{"ok":0,"warning":0,"error":0},"allocation":{"allocated":0,"allocatable":0,"unusable":0,"unknown":0}},"facets":{"nodes":[{"uuid":"u","name":"gpu3"}],"models":["H100"],"vendors":["nvidia"]}}`
	if string(got) != want {
		t.Fatalf("wire JSON\n got: %s\nwant: %s", got, want)
	}
	var back dto.GPUPage
	if err := json.Unmarshal(got, &back); err != nil || !reflect.DeepEqual(back, page) {
		t.Fatalf("round trip = %+v err %v", back, err)
	}
}
