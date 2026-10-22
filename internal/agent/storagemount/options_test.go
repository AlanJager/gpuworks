package storagemount

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/storagerules"
)

// The rule itself is tested in storagerules; the mount manager must apply that
// one, not a copy.
func TestValidateOptionsIsTheSharedRule(t *testing.T) {
	for _, in := range []string{"", " ro , vers=4.2 ", "rw,rw,hard", "rw,suid", "ro,rw", "vers=", "rw;x"} {
		want, wantErr := storagerules.ValidateMountOptions(in)
		got, err := ValidateOptions(in)
		if got != want || (err == nil) != (wantErr == nil) {
			t.Errorf("ValidateOptions(%q) = %q, %v; the shared rule says %q, %v", in, got, err, want, wantErr)
		}
	}
}

func TestEffectiveOptions(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "rw,noatime",
		"vers=4.2":           "rw,vers=4.2,noatime",
		"ro":                 "ro,noatime",
		"ro,vers=3":          "ro,vers=3,noatime",
		"rw,relatime":        "rw,relatime",
		"atime,hard":         "rw,atime,hard",
		"strictatime,vers=4": "rw,strictatime,vers=4",
		"noatime,rw":         "noatime,rw",
		"rw,rw,noatime":      "rw,noatime",
	} {
		got, err := effectiveOptions(in)
		if err != nil || got != want {
			t.Errorf("effectiveOptions(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := effectiveOptions("bogus"); err == nil {
		t.Error("invalid options must be rejected")
	}
}
