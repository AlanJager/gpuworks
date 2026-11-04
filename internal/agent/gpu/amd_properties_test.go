package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// KFD node_show emits unique_id/debug_prop with %llu. They need not fit an
// int64, and neither field participates in GPUWorks' PCI/render matching.
// This is synthetic kernel ABI data, not a capture from an AMD machine.
func TestAMDPropertiesIgnoreUnconsumedKernelMetadata(t *testing.T) {
	p, files := amd395UMAFixture()
	files["/sys/class/kfd/kfd/topology/nodes/1/properties"] += "unique_id 18446744073709551615\ndebug_prop 9223372036854775808\n"
	cards, err := p.Probe(context.Background())
	if err != nil || len(cards) != 1 {
		t.Fatalf("Probe returned %d cards: %v", len(cards), err)
	}
	if !cards[0].Healthy || cards[0].ArchID != "gfx1151" || cards[0].MemoryTotalBytes != umaTotal {
		t.Fatalf("unconsumed kernel metadata invalidated compute identity: %+v", cards[0])
	}
}

func TestAMDComputeCandidatesWithUnsignedMetadata(t *testing.T) {
	root := t.TempDir()
	writeBusCard(t, root, "0000:43:00.0", "0x1002", "0x1586", "0x030000", "amdgpu")
	writeAMDCensusCompute(t, root)
	path := filepath.Join(root, "sys/class/kfd/kfd/topology/nodes/1/properties")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\nunique_id 18446744073709551615\ndebug_prop 9223372036854775808\n")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	candidates := amdComputePCIFunctions(context.Background(), root, os.ReadFile, filepath.Glob)
	if !candidates["0000:43:00.0"] {
		t.Fatal("valid KFD compute function omitted from vendor census")
	}
}

func TestAMDPropertiesInvalidConsumedFactsFailClosed(t *testing.T) {
	for _, bad := range []string{"vendor_id missing", "domain -1", "location_id 9223372036854775808", "drm_render_minor", "gfx_target_version 110501 extra", "vendor_id 4098\nvendor_id 4098"} {
		t.Run(strings.ReplaceAll(bad, "\n", "/"), func(t *testing.T) {
			if _, err := parseAMDProperties(bad); err == nil {
				t.Fatalf("accepted malformed or ambiguous consumed property %q", bad)
			}
		})
	}
}
