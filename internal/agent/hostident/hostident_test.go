package hostident

import (
	"testing"
	"testing/fstest"
)

// The Linux UUID comes from sysfs, normalized; a missing (non-root, no
// SMBIOS) or placeholder value is reported as unknown, never guessed.
func TestReadDMI(t *testing.T) {
	for name, tc := range map[string]struct {
		fs   fstest.MapFS
		want string
	}{
		"present":     {fstest.MapFS{dmiProductUUID: {Data: []byte("4C4C4544-0042-3510-8051-B4C04F4E3432\n")}}, "4c4c4544-0042-3510-8051-b4c04f4e3432"},
		"unreadable":  {fstest.MapFS{}, ""},
		"empty":       {fstest.MapFS{dmiProductUUID: {Data: []byte("\n")}}, ""},
		"placeholder": {fstest.MapFS{dmiProductUUID: {Data: []byte("03000200-0400-0500-0006-000700080009\n")}}, ""},
		"zeros":       {fstest.MapFS{dmiProductUUID: {Data: []byte("00000000-0000-0000-0000-000000000000\n")}}, ""},
	} {
		if got := readDMI(tc.fs); got != tc.want {
			t.Errorf("%s: readDMI = %q, want %q", name, got, tc.want)
		}
	}
}

func TestParseIORegPlatformUUID(t *testing.T) {
	out := `+-o J316sAP  <class IOPlatformExpertDevice, id 0x100000202, registered, matched, active, busy 0 (12 ms), retain 39>
    {
      "IOPlatformSerialNumber" = "C02XXXXXXXXX"
      "IOPlatformUUID" = "6A3B1C2D-4E5F-6071-8293-A4B5C6D7E8F9"
      "model" = <"MacBookPro18,1">
    }
`
	if got := parseIORegPlatformUUID(out); got != "6a3b1c2d-4e5f-6071-8293-a4b5c6d7e8f9" {
		t.Fatalf("parse = %q", got)
	}
	if got := parseIORegPlatformUUID(`"IOPlatformSerialNumber" = "C02"`); got != "" {
		t.Fatalf("no UUID line parsed as %q", got)
	}
	if got := parseIORegPlatformUUID(""); got != "" {
		t.Fatalf("empty output parsed as %q", got)
	}
}

// SystemUUID is read once per process and never panics, whatever the host.
func TestSystemUUIDIsStable(t *testing.T) {
	if a, b := SystemUUID(), SystemUUID(); a != b {
		t.Fatalf("SystemUUID changed within one process: %q then %q", a, b)
	}
}
