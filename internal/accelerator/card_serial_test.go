package accelerator

import "testing"

// Each probe's id is classified as the card's own hardware identity or as
// one made up from its slot or index (the forms the agent's probers write).
func TestCardSerialTellsHardwareIDsFromSlotIDs(t *testing.T) {
	for _, tc := range []struct {
		uuid string
		ok   bool
	}{
		{"GPU-8f3c1a2b-4d5e-6f70-8192-a3b4c5d6e7f8", true}, // NVIDIA
		{"GPU-0a1b2c3d-0000-1111-2222-333344445555", true}, // PPU
		{"GPU-623469db-fc46-50bd-8cd0-0631cee1dcc6", true}, // Kunlunxin
		{"amd-3d1a2b3c4d5e6f70", true},                     // AMD unique_id
		{"amd-0000:3b:00.0", false},                        // AMD without unique_id
		{"GPU-5e6f708192a3b4c5", true},                     // Hygon unique id
		{"hygon-0000:3b:00.0", false},
		{"hygon-2", false},
		{"ascend-3", false},
		{"Ascend-3", false},
		{"APPLE-6A3B1C2D-4E5F-6071-8293-A4B5C6D7E8F9", true},
		{"GPU-FAKE-0", false},
		{"FAKE-NVIDIA-1", false},
		{"", false},
		{"   ", false},
	} {
		serial, ok := CardSerial(tc.uuid)
		if ok != tc.ok {
			t.Errorf("CardSerial(%q) ok = %v, want %v", tc.uuid, ok, tc.ok)
		}
		if ok && serial != tc.uuid {
			t.Errorf("CardSerial(%q) = %q, want the value itself", tc.uuid, serial)
		}
		if !ok && serial != "" {
			t.Errorf("CardSerial(%q) = %q for a slot id", tc.uuid, serial)
		}
	}
}
