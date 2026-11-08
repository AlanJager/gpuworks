package proto

import "testing"

func TestIsEnginePortRefusal(t *testing.T) {
	for msg, want := range map[string]bool{
		EnginePortRefused + ": port 30000 is already in use on this node; nothing was started": true,
		EnginePortInUse + ": port 30000 is already in use on this node":                        false,
		"rank 1 on n failed: " + EnginePortRefused + ": port 30000":                            false,
		EnginePortRefused:      false,
		"":                     false,
		"image pull timed out": false,
	} {
		if got := IsEnginePortRefusal(msg); got != want {
			t.Errorf("IsEnginePortRefusal(%q) = %v, want %v", msg, got, want)
		}
	}
}
