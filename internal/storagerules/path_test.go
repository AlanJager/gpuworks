package storagerules

import "testing"

func TestValidateStoragePath(t *testing.T) {
	for _, p := range []string{"/", "/etc", "/etc/ssl", "/mnt/../etc", "/mnt/gpuworks/link/../etc", "/data/models/./weights", "/data/models/../weights", "/proc/1/root", "/var/run", "/var/lib/docker/volumes", "/usr/local", "relative", "/data/\x00weights"} {
		if ValidatePath(p) == nil {
			t.Errorf("unsafe path accepted: %q", p)
		}
	}
	for _, p := range []string{"/data/models", "/mnt/gpuworks/a", "/opt/gpuworks/models", "/srv/weights", "/etc-models"} {
		if err := ValidatePath(p); err != nil {
			t.Errorf("model path denied: %s %v", p, err)
		}
	}
	if Within("/data/models", "/data/models-other") || Within("/data/models", "/data/models/../../etc") {
		t.Fatal("path-prefix boundary bypass")
	}
}
