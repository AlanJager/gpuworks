package gpu

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const (
	umaDevicePath = "/sys/class/drm/card2/device"
	umaTotal      = int64(51539607552)
)

// amd395UMAFixture is synthetic ABI data, not a raw capture from an AMD host.
// Historical smoke results cannot validate additional kernel topology variants.
func amd395UMAFixture() (*AMDProber, map[string]string) {
	f := amdFiles()
	for key, value := range map[string]string{
		umaDevicePath + "/device":                                    "0x1586",
		umaDevicePath + "/uevent":                                    "PCI_SLOT_NAME=0000:c5:00.0",
		umaDevicePath + "/mem_info_vram_total":                       "536870912",
		umaDevicePath + "/mem_info_vram_used":                        "1048576",
		umaDevicePath + "/mem_info_gtt_total":                        fmt.Sprint(umaTotal),
		umaDevicePath + "/mem_info_gtt_used":                         "4194304",
		umaDevicePath + "/drm/renderD128/dev":                        "226:128",
		"/sys/class/kfd/kfd/topology/nodes/1/properties":             "vendor_id 4098\ndevice_id 5510\ndomain 0\nlocation_id 50432\ndrm_render_minor 128\ngfx_target_version 110501\nlocal_mem_size 0\nmem_banks_count 1\n",
		"/sys/class/kfd/kfd/topology/nodes/1/mem_banks/0/properties": "heap_type 1\nsize_in_bytes 51539607552\n",
	} {
		f[key] = value
	}
	p := testAMD(f)
	p.glob = func(pattern string) ([]string, error) {
		switch {
		case pattern == "/sys/class/drm/card[0-9]*/device":
			return []string{umaDevicePath, "/sys/class/drm/card4/device"}, nil
		case strings.HasSuffix(pattern, "/drm/renderD*"):
			return []string{umaDevicePath + "/drm/renderD128"}, nil
		case pattern == "/sys/class/kfd/kfd/topology/nodes/*/properties":
			return []string{"/sys/class/kfd/kfd/topology/nodes/1/properties"}, nil
		default:
			return nil, nil
		}
	}
	return p, f
}

func TestAMD395UsesMatchedKFDGTTCapacityAndUsage(t *testing.T) {
	p, _ := amd395UMAFixture()
	probe, err := p.Probe(context.Background())
	if err != nil || len(probe) != 1 {
		t.Fatalf("Probe = %v, %v", probe, err)
	}
	if probe[0].MemoryTotalBytes != umaTotal || probe[0].HealthMessage != "" || p.lastSeenMem[probe[0].Index].source != amdMemoryUMAPool {
		t.Fatalf("UMA inventory = %+v", probe[0])
	}
	metrics, err := p.Metrics(context.Background())
	if err != nil || len(metrics) != 1 {
		t.Fatalf("Metrics = %v, %v", metrics, err)
	}
	if got := metrics[0].Values["gpuworks_gpu_memory_total_bytes"]; got != float64(umaTotal) {
		t.Fatalf("metric total = %v", got)
	}
	if got := metrics[0].Values["gpuworks_gpu_memory_used_bytes"]; got != 4194304 {
		t.Fatalf("metric used = %v, want KFD/GTT usage", got)
	}
}

func TestAMD395UnknownUMADoesNotPublishCarveoutAsHealthy(t *testing.T) {
	p, files := amd395UMAFixture()
	delete(files, "/sys/class/kfd/kfd/topology/nodes/1/mem_banks/0/properties")
	gs, err := p.Probe(context.Background())
	if err != nil || len(gs) != 1 || gs[0].Healthy || gs[0].MemoryTotalBytes != 0 || !strings.Contains(gs[0].HealthMessage, "UMA capacity") {
		t.Fatalf("unverified UMA inventory = %+v, %v", gs, err)
	}
	if p.Available() {
		t.Fatal("unverified UMA card auto-selected")
	}
}

func TestAMDMemoryPoolOverrideFailsClosed(t *testing.T) {
	tests := []struct {
		name                 string
		allowEmptyDiagnostic bool
		mutate               func(map[string]string)
	}{
		{name: "other device with zero local memory", allowEmptyDiagnostic: true, mutate: func(f map[string]string) {
			f[umaDevicePath+"/device"] = "0x744c"
			f["/proc/meminfo"] = "MemTotal: 134217728 kB\n"
		}},
		{name: "missing local memory fact", mutate: func(f map[string]string) {
			f["/sys/class/kfd/kfd/topology/nodes/1/properties"] = strings.ReplaceAll(f["/sys/class/kfd/kfd/topology/nodes/1/properties"], "local_mem_size 0\n", "")
		}},
		{name: "wrong KFD PCI location", mutate: func(f map[string]string) {
			f["/sys/class/kfd/kfd/topology/nodes/1/properties"] = strings.ReplaceAll(f["/sys/class/kfd/kfd/topology/nodes/1/properties"], "location_id 50432", "location_id 50433")
		}},
		{name: "wrong KFD gfx target", mutate: func(f map[string]string) {
			f["/sys/class/kfd/kfd/topology/nodes/1/properties"] = strings.ReplaceAll(f["/sys/class/kfd/kfd/topology/nodes/1/properties"], "gfx_target_version 110501", "gfx_target_version 110500")
		}},
		{name: "wrong render minor", mutate: func(f map[string]string) {
			f["/sys/class/kfd/kfd/topology/nodes/1/properties"] = strings.ReplaceAll(f["/sys/class/kfd/kfd/topology/nodes/1/properties"], "drm_render_minor 128", "drm_render_minor 129")
		}},
		{name: "unexpected bank count", mutate: func(f map[string]string) {
			f["/sys/class/kfd/kfd/topology/nodes/1/properties"] = strings.ReplaceAll(f["/sys/class/kfd/kfd/topology/nodes/1/properties"], "mem_banks_count 1", "mem_banks_count 2")
		}},
		{name: "wrong heap type", mutate: func(f map[string]string) {
			f["/sys/class/kfd/kfd/topology/nodes/1/mem_banks/0/properties"] = strings.ReplaceAll(f["/sys/class/kfd/kfd/topology/nodes/1/mem_banks/0/properties"], "heap_type 1", "heap_type 0")
		}},
		{name: "bank and GTT disagree", mutate: func(f map[string]string) { f["/sys/class/drm/card2/device/mem_info_gtt_total"] = "51539607551" }},
		{name: "bank size unavailable", mutate: func(f map[string]string) { delete(f, "/sys/class/kfd/kfd/topology/nodes/1/mem_banks/0/properties") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, f := amd395UMAFixture()
			tt.mutate(f)
			facts := p.memoryFacts(umaDevicePath, "0000:c5:00.0", "gfx1151", 536870912)
			if facts.source != amdMemoryDedicatedVRAM || facts.totalBytes != 536870912 || (!tt.allowEmptyDiagnostic && facts.diagnostic == "") {
				t.Fatalf("unsafe UMA fallback facts = %+v", facts)
			}
		})
	}
}

func TestAMDBDFLocationRejectsOutOfRangeComponents(t *testing.T) {
	for _, bdf := range []string{"-001:00:00.0", "10000:00:00.0", "0000:-01:00.0", "0000:00:-1.0", "0000:00:00.-1", "0000:100:00.0"} {
		if _, _, ok := amdBDFLocation(bdf); ok {
			t.Errorf("amdBDFLocation(%q) accepted invalid address", bdf)
		}
	}
}

func TestAMDMemoryMetricsOmitUsedWhenPublishedCapacityChanges(t *testing.T) {
	p, f := amd395UMAFixture()
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	f[umaDevicePath+"/mem_info_gtt_total"] = "51539607551"
	metrics, err := p.Metrics(context.Background())
	if err != nil || len(metrics) != 1 {
		t.Fatalf("Metrics = %v, %v", metrics, err)
	}
	if _, ok := metrics[0].Values["gpuworks_gpu_memory_used_bytes"]; ok {
		t.Fatal("usage from changed/fallback capacity was reported")
	}
}
