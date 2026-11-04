package gpu

import (
	"context"
	"testing"
)

// Cloud virtual functions expose no product_name; known Instinct device IDs
// are named, while the driver's product_name still wins when present.
func TestAMDNamesInstinctWithoutProductName(t *testing.T) {
	const p = "/sys/class/drm/card2/device/"
	for _, tc := range []struct{ device, product, want string }{
		{"0x74b9", "", "AMD Instinct MI325X"},
		{"0x74A1\n", "", "AMD Instinct MI300X"},
		{"0x74b9", "AMD Instinct MI325X OAM", "AMD Instinct MI325X OAM"},
		{"0x7551", "", "AMD GPU 0x7551"}, // KFD device_id no longer matches, so no arch suffix
	} {
		files := amdFiles()
		files[p+"device"] = tc.device
		if tc.product == "" {
			delete(files, p+"product_name")
		} else {
			files[p+"product_name"] = tc.product
		}
		gs, err := testAMD(files).Probe(context.Background())
		if err != nil || len(gs) != 1 {
			t.Fatalf("%s: inventory %v %v", tc.device, gs, err)
		}
		if gs[0].Name != tc.want {
			t.Fatalf("%q/%q: name = %q, want %q", tc.device, tc.product, gs[0].Name, tc.want)
		}
	}
}
