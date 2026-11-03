package gpu

import (
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// hygonRASFiles serves a card's RAS directory from a map; a path not in it
// does not exist. The healthy values are as read on the K100 host
// (192.0.2.133, 2026-10-30).
func hygonRASFiles(files map[string]string) func(string) ([]byte, error) {
	base := map[string]string{
		"umc_err_count": "ue: 0\nce: 0\n", "gfx_err_count": "ue: 0\nce: 0\n",
		"sdma_err_count": "ue: 0\nce: 0\n", "gpu_vram_bad_pages": "",
	}
	for k, v := range files {
		base[k] = v
	}
	return func(path string) ([]byte, error) {
		const dir = "/sys/bus/pci/devices/0000:67:00.0/ras/"
		if v, ok := base[strings.TrimPrefix(path, dir)]; ok && strings.HasPrefix(path, dir) {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}
}

func TestHygonHealth(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		code  string
		field string
	}{
		{"healthy", nil, "", ""},
		{"correctable only", map[string]string{"umc_err_count": "ue: 0\nce: 12\n"}, "", ""},
		{"memory uncorrectable", map[string]string{"umc_err_count": "ue: 1\nce: 3\n"}, accelerator.HealthMemoryUncorrectable, "ras.umc.ue=1"},
		{"other block uncorrectable", map[string]string{"gfx_err_count": "ue: 2\nce: 0\n"}, accelerator.HealthVendorFault, "ras.gfx.ue=2"},
		{"page pending", map[string]string{"gpu_vram_bad_pages": "0x00001234 : 0x00001000 : P\n"}, accelerator.HealthMemoryRepairPending, "bad_pages.pending=1"},
		{"page failed", map[string]string{"gpu_vram_bad_pages": "0x00001234 : 0x00001000 : F\n0x00001235 : 0x00001000 : R\n"}, accelerator.HealthMemoryRemapFailed, "bad_pages.failed=1"},
		{"retired only", map[string]string{"gpu_vram_bad_pages": "0x00001234 : 0x00001000 : R\n0x00002000 : 0x00001000 : R\n"}, accelerator.HealthMemoryErrorHistory, "bad_pages.retired=2"},
		{"memory outranks pages", map[string]string{"umc_err_count": "ue: 1\nce: 0\n", "gpu_vram_bad_pages": "0x1 : 0x1000 : F\n"}, accelerator.HealthMemoryUncorrectable, "bad_pages.failed=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, evidence := hygonHealth("0000:67:00.0", hygonRASFiles(tc.files))
			if code != tc.code {
				t.Fatalf("code = %q, want %q (evidence %v)", code, tc.code, evidence)
			}
			if tc.field != "" && !strings.Contains(accelerator.FormatHealthEvidence(evidence), tc.field) {
				t.Fatalf("evidence %q lacks %q", accelerator.FormatHealthEvidence(evidence), tc.field)
			}
		})
	}
}

func TestHygonHealthWithoutRASFiles(t *testing.T) {
	// A driver without RAS files, or a card without an address, gives no
	// evidence either way: no finding.
	none := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	if code, ev := hygonHealth("0000:67:00.0", none); code != "" || ev != nil {
		t.Fatalf("hygonHealth = %q %v", code, ev)
	}
	if code, _ := hygonHealth("", hygonRASFiles(map[string]string{"umc_err_count": "ue: 1\n"})); code != "" {
		t.Fatalf("a card without an address got %q", code)
	}
}

func TestHygonHealthGuide(t *testing.T) {
	g, ok := accelerator.HealthGuideFor(accelerator.HealthCard{Vendor: accelerator.VendorHygon, Code: accelerator.HealthMemoryUncorrectable, BDF: "0000:67:00.0"}, nil)
	if !ok || g.Probe != "grep . /sys/bus/pci/devices/0000:67:00.0/ras/*_err_count" || g.Recheck != "hy-smi --showrasinfo all" || len(g.Actions) != 0 {
		t.Fatalf("guide = %+v ok=%v", g, ok)
	}
	g, ok = accelerator.HealthGuideFor(accelerator.HealthCard{Vendor: accelerator.VendorHygon, Code: accelerator.HealthMemoryRepairPending, BDF: "0000:67:00.0"}, nil)
	if !ok || g.Recheck != "hy-smi --showpagesinfo" {
		t.Fatalf("guide = %+v ok=%v", g, ok)
	}
}
