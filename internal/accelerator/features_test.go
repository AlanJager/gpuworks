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
	if missing := Unsupported(VendorAMD, FeatureDataParallel, FeaturePipelineParallel); len(missing) != 0 {
		t.Fatalf("AMD lacks %v, want none", missing)
	}
	if RequiresKnownLaunchFeatures(VendorAMD) {
		t.Fatal("AMD has no partial restriction left, so an unknown launch needs no proof")
	}
	if missing := Unsupported("Apple", all...); !slices.Equal(missing, all) {
		t.Fatalf("apple lacks %v, want all of %v", missing, all)
	}
	if missing := Unsupported(VendorApple); missing != nil {
		t.Fatalf("asking for nothing = %v", missing)
	}
}

// A partial restriction (no vendor carries one today) refuses exactly its
// features and requires a known launch command.
func TestPartialRestrictionRefusesOnlyItsFeatures(t *testing.T) {
	vendor := VendorHygon
	saved, had := targetConstraints[vendor]
	targetConstraints[vendor] = targetConstraint{unsupported: []Feature{FeatureDataParallel}}
	t.Cleanup(func() {
		if had {
			targetConstraints[vendor] = saved
		} else {
			delete(targetConstraints, vendor)
		}
	})
	if missing := Unsupported(vendor, FeatureDataParallel, FeatureEngineSleep); !slices.Equal(missing, []Feature{FeatureDataParallel}) {
		t.Fatalf("partial restriction lacks %v, want data parallelism only", missing)
	}
	if !RequiresKnownLaunchFeatures(vendor) {
		t.Fatal("a partial restriction must require a known launch")
	}
}
