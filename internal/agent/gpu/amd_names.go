package gpu

import "strings"

// amdInstinctNames names Instinct accelerators by PCI device ID for hosts
// whose amdgpu driver exposes no product_name (common for virtual
// functions on cloud GPU droplets). IDs come from the PCI ID repository,
// except 0x74b9, observed on MI325X cloud instances. A device not listed
// keeps the "AMD GPU 0x…" fallback name.
var amdInstinctNames = map[string]string{
	"0x738c": "AMD Instinct MI100",
	"0x738e": "AMD Instinct MI100",
	"0x7408": "AMD Instinct MI250X",
	"0x740c": "AMD Instinct MI250X/MI250",
	"0x740f": "AMD Instinct MI210",
	"0x74a0": "AMD Instinct MI300A",
	"0x74a1": "AMD Instinct MI300X",
	"0x74b5": "AMD Instinct MI300X",
	"0x74a5": "AMD Instinct MI325X",
	"0x74b9": "AMD Instinct MI325X",
}

func amdInstinctName(device string) string {
	return amdInstinctNames[strings.ToLower(strings.TrimSpace(device))]
}
