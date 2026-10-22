package api

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

var gpuAllocationStates = []string{"available", "partial", "allocated", "unusable", "unknown"}

// gpuListSorters are the orders GET /gpus offers, by the GPU list's column
// keys. Rows without a reading sort before rows with one.
var gpuListSorters = listSorters[dto.PhysicalGPU]{
	"node": func(a, b dto.PhysicalGPU) int {
		return cmp.Or(compareText(a.NodeName, b.NodeName), cmp.Compare(a.NodeUUID, b.NodeUUID), cmp.Compare(a.Index, b.Index))
	},
	"model":            func(a, b dto.PhysicalGPU) int { return compareText(a.Name, b.Name) },
	"vendor":           func(a, b dto.PhysicalGPU) int { return cmp.Compare(a.Vendor, b.Vendor) },
	"allocation_state": func(a, b dto.PhysicalGPU) int { return cmp.Compare(a.AllocationState, b.AllocationState) },
	"occupants":        func(a, b dto.PhysicalGPU) int { return cmp.Compare(len(a.Occupants), len(b.Occupants)) },
	"utilization":      func(a, b dto.PhysicalGPU) int { return compareReading(a.Load.UtilizationPct, b.Load.UtilizationPct) },
	"memory":           func(a, b dto.PhysicalGPU) int { return compareReading(a.Load.MemoryUsed, b.Load.MemoryUsed) },
	"temperature":      func(a, b dto.PhysicalGPU) int { return compareReading(a.Load.TemperatureC, b.Load.TemperatureC) },
	"power":            func(a, b dto.PhysicalGPU) int { return compareReading(a.Load.PowerWatts, b.Load.PowerWatts) },
	"fan":              func(a, b dto.PhysicalGPU) int { return compareReading(a.Load.FanSpeedPct, b.Load.FanSpeedPct) },
	"bdf":              func(a, b dto.PhysicalGPU) int { return cmp.Compare(a.PCIeBDF, b.PCIeBDF) },
	"numa":             func(a, b dto.PhysicalGPU) int { return cmp.Compare(a.NUMANode, b.NUMANode) },
}

func compareReading(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return cmp.Compare(*a, *b)
}

// gpuListFilter is GET /gpus's column filters: each names alternatives, an
// empty one matches every GPU.
type gpuListFilter struct {
	nodes, vendors, models, allocation map[string]bool
}

func (f gpuListFilter) matches(gpu *dto.PhysicalGPU) bool {
	in := func(set map[string]bool, value string) bool { return len(set) == 0 || set[value] }
	return in(f.nodes, gpu.NodeUUID) && in(f.vendors, gpu.Vendor) && in(f.models, gpu.Name) &&
		in(f.allocation, gpu.AllocationState)
}

// listValues reads a comma-separated filter. With known values, one outside
// them is refused, as an unknown state is.
func listValues(r *http.Request, name string, known []string) (map[string]bool, error) {
	raw, ok := listParam(r.URL.Query(), name)
	if !ok || raw == "" {
		return nil, nil
	}
	set := map[string]bool{}
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value == "" || (known != nil && !slices.Contains(known, value)) {
			return nil, listFilterError(name, value)
		}
		set[value] = true
	}
	return set, nil
}

func parseGPUListFilter(r *http.Request) (gpuListFilter, error) {
	var f gpuListFilter
	var err error
	for _, field := range []struct {
		name  string
		known []string
		set   *map[string]bool
	}{
		{"node", nil, &f.nodes}, {"vendor", nil, &f.vendors}, {"model", nil, &f.models},
		{"allocation_state", gpuAllocationStates, &f.allocation},
	} {
		if *field.set, err = listValues(r, field.name, field.known); err != nil {
			return f, err
		}
	}
	return f, nil
}

// gpuPage answers a paged GET /gpus from the cached inventory: q (a substring
// of the node name or uuid, the model, the GPU uuid or the PCIe address), the
// column filters and sort cut the whole fleet before the page is taken. The
// overview and the filter facets always cover every GPU.
func gpuPage(inventory dto.GPUListResponse, query listQuery, filter gpuListFilter, order listSort) dto.GPUPage {
	matched := make([]dto.PhysicalGPU, 0, len(inventory.Items))
	facets := dto.GPUFacets{Nodes: []dto.GPUFacetNode{}, Models: []string{}, Vendors: []string{}}
	seenNode, seenModel, seenVendor := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range inventory.Items {
		gpu := &inventory.Items[i]
		if !seenNode[gpu.NodeUUID] {
			seenNode[gpu.NodeUUID] = true
			facets.Nodes = append(facets.Nodes, dto.GPUFacetNode{UUID: gpu.NodeUUID, Name: gpu.NodeName})
		}
		if !seenModel[gpu.Name] {
			seenModel[gpu.Name] = true
			facets.Models = append(facets.Models, gpu.Name)
		}
		if !seenVendor[gpu.Vendor] {
			seenVendor[gpu.Vendor] = true
			facets.Vendors = append(facets.Vendors, gpu.Vendor)
		}
		if query.matchesSearch(gpu.NodeName, gpu.NodeUUID, gpu.Name, gpu.UUID, gpu.PCIeBDF) && filter.matches(gpu) {
			matched = append(matched, *gpu)
		}
	}
	slices.SortFunc(facets.Nodes, func(a, b dto.GPUFacetNode) int {
		return cmp.Or(compareText(a.Name, b.Name), cmp.Compare(a.UUID, b.UUID))
	})
	slices.SortFunc(facets.Models, compareText)
	slices.Sort(facets.Vendors)
	gpuListSorters.apply(matched, order)
	page := pageOf(matched, query, func(rows []dto.PhysicalGPU) []dto.PhysicalGPU { return slices.Clone(rows) })
	return dto.GPUPage{ListPage: page, Overview: inventory.Overview, Facets: facets}
}
