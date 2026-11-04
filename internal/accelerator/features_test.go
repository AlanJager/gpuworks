package accelerator

import (
	"slices"
	"testing"
)

func TestFeatureSupportByVendor(t *testing.T) {
	all := []Feature{FeatureEngineSleep, FeatureMultiNode, FeaturePD, FeatureKVDiskTier}
	for _, vendor := range []string{VendorNVIDIA, VendorAscend, VendorPPU, VendorHygon, ""} {
		if missing := Unsupported(vendor, all...); len(missing) != 0 {
			t.Errorf("%q lacks %v", vendor, missing)
		}
	}
	if missing := Unsupported(VendorAMD, all...); len(missing) != 0 {
		t.Fatalf("AMD lacks %v, want none of %v", missing, all)
	}
	if missing := Unsupported(VendorAMD, FeatureDataParallel, FeatureEngineSleep); !slices.Equal(missing, []Feature{FeatureDataParallel}) {
		t.Fatalf("AMD lacks %v, want data parallelism only", missing)
	}
	if missing := Unsupported("Apple", all...); !slices.Equal(missing, all) {
		t.Fatalf("apple lacks %v, want all of %v", missing, all)
	}
	if missing := Unsupported(VendorApple); missing != nil {
		t.Fatalf("asking for nothing = %v", missing)
	}
}
