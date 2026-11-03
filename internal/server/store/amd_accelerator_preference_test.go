package store

import "testing"

func TestNormalizeAMDAcceleratorPreference(t *testing.T) {
	got, ok := NormalizeAcceleratorPreference(" AMD ")
	if !ok || got != "amd" {
		t.Fatalf("NormalizeAcceleratorPreference(AMD) = %q, %v", got, ok)
	}
}
