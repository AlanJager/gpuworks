package app

import (
	"bytes"
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/usagereport"
	"gpuworks.ai/gpuworks/internal/version"
)

// The report names the build's edition beside its version, so the program can
// tell builds apart without the version line ever carrying it.
func TestUsageReportNamesTheEdition(t *testing.T) {
	saved := version.Edition
	t.Cleanup(func() { version.Edition = saved })
	for _, edition := range []string{"community", "oem-x"} {
		version.Edition = edition
		rig := newUsageRig(t)
		day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
		r, err := rig.usage.build(context.Background(), day, day.Add(30*time.Hour), nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.Server.Edition != edition {
			t.Fatalf("server.edition = %q, want %q", r.Server.Edition, edition)
		}
		raw, err := usagereport.Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte(`"edition":"`+edition+`"`)) {
			t.Fatalf("the encoded report does not carry the edition: %s", raw)
		}
	}
}
