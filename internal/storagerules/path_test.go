package storagerules

import (
	"errors"
	"testing"
)

func TestValidateStoragePath(t *testing.T) {
	denied := DefaultDeniedPaths()
	for _, p := range []string{"/", "/etc", "/etc/ssl", "/mnt/../etc", "/mnt/gpuworks/link/../etc", "/data/models/./weights", "/data/models/../weights", "/proc/1/root", "/var/run", "/var/lib/docker/volumes", "/usr/local", "relative", "/data/\x00weights"} {
		if ValidatePath(p, denied) == nil {
			t.Errorf("unsafe path accepted: %q", p)
		}
	}
	for _, p := range []string{"/data/models", "/mnt/gpuworks/a", "/opt/gpuworks/models", "/srv/weights", "/etc-models"} {
		if err := ValidatePath(p, denied); err != nil {
			t.Errorf("model path denied: %s %v", p, err)
		}
	}
	if Within("/data/models", "/data/models-other") || Within("/data/models", "/data/models/../../etc") {
		t.Fatal("path-prefix boundary bypass")
	}
}

// A replaced denylist frees the built-in directories and denies its own,
// while the form rules hold for any list, including an empty one.
func TestValidateStoragePathWithReplacedDenylist(t *testing.T) {
	denied := []string{"/opt/gpuworks"}
	if err := ValidatePath("/usr/local/models", denied); err != nil {
		t.Errorf("built-in directory still denied after replacement: %v", err)
	}
	for _, p := range []string{"/opt/gpuworks", "/opt/gpuworks/bin"} {
		if err := ValidatePath(p, denied); !errors.Is(err, ErrDeniedPath) {
			t.Errorf("configured denied path %s: err = %v, want ErrDeniedPath", p, err)
		}
	}
	if err := ValidatePath("/opt/gpuworks-models", denied); err != nil {
		t.Errorf("similar prefix denied: %v", err)
	}
	// A malformed path is refused for its form, never as a denied directory,
	// even when it would also fall under one.
	for _, list := range [][]string{nil, {"/etc"}} {
		for _, p := range []string{"/", "relative", "/data/../etc", "/data/\x01", "/etc/../x"} {
			if err := ValidatePath(p, list); err == nil || errors.Is(err, ErrDeniedPath) {
				t.Errorf("malformed path %q with denylist %v: err = %v", p, list, err)
			}
		}
	}
	list := DefaultDeniedPaths()
	list[0] = "/changed"
	if DefaultDeniedPaths()[0] != "/etc" {
		t.Fatal("DefaultDeniedPaths exposes the shared slice")
	}
}
