package gpu

import "testing"

func TestFromEnvParsesFakeGPUShape(t *testing.T) {
	p, err := FromEnv("A100x4,24576")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || len(p.GPUs) != 4 || p.GPUs[0].Name != "A100" || p.GPUs[0].MemoryTotalBytes != 24576<<20 {
		t.Fatalf("prober = %+v", p)
	}
}

func TestFromEnvRejectsMalformedSpec(t *testing.T) {
	for _, value := range []string{"A100", "A100x0", "A100x2,-1", "A100x999"} {
		if _, err := FromEnv(value); err == nil {
			t.Fatalf("%q unexpectedly parsed", value)
		}
	}
}

// An NVIDIA spec predicate matches on compute capability, so a fake NVIDIA card
// that reports none would make every cc-gated inference spec look incompatible
// and hide the whole deploy flow from a hermetic environment.
func TestFromEnvReportsComputeCapabilityForNVIDIAFakes(t *testing.T) {
	t.Setenv("GPUWORKS_FAKE_GPU_VENDOR", "nvidia")
	t.Setenv("GPUWORKS_FAKE_GPU_ARCH", "7.5")
	p, err := FromEnv("Tesla T4x2,16384")
	if err != nil {
		t.Fatal(err)
	}
	for _, gpu := range p.GPUs {
		if gpu.ComputeCapability != "7.5" {
			t.Fatalf("compute capability = %q, want the arch id 7.5", gpu.ComputeCapability)
		}
		if gpu.ArchID != "7.5" {
			t.Fatalf("arch id = %q", gpu.ArchID)
		}
	}
}

func TestFromEnvHonorsAnExplicitComputeCapabilityAndSkipsNonNVIDIA(t *testing.T) {
	// A vendor whose arch id is not a compute capability must not have the arch
	// id leak into the cc field.
	t.Setenv("GPUWORKS_FAKE_GPU_VENDOR", "ascend")
	t.Setenv("GPUWORKS_FAKE_GPU_ARCH", "910B4")
	p, err := FromEnv("Ascend 910B4x1,65536")
	if err != nil {
		t.Fatal(err)
	}
	if p.GPUs[0].ComputeCapability != "" {
		t.Fatalf("non-NVIDIA compute capability = %q, want empty", p.GPUs[0].ComputeCapability)
	}
	if p.GPUs[0].ArchID != "910B4" {
		t.Fatalf("arch id = %q", p.GPUs[0].ArchID)
	}
	// An explicit override wins for any vendor.
	t.Setenv("GPUWORKS_FAKE_GPU_CC", "9.0")
	t.Setenv("GPUWORKS_FAKE_GPU_VENDOR", "nvidia")
	t.Setenv("GPUWORKS_FAKE_GPU_ARCH", "hopper")
	p, err = FromEnv("H100x1,81920")
	if err != nil {
		t.Fatal(err)
	}
	if p.GPUs[0].ComputeCapability != "9.0" || p.GPUs[0].ArchID != "hopper" {
		t.Fatalf("explicit cc override = %+v", p.GPUs[0])
	}
}
