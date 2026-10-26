package accelerator

import (
	"reflect"
	"testing"
)

func TestRegistryExhaustive(t *testing.T) {
	want := []struct {
		id       string
		vendor   string
		maturity Maturity
		assets   []string
	}{
		{BackendNVIDIALogical, VendorNVIDIA, MaturityStable, []string{"libvgpu.so"}},
		{BackendAscendLogical, VendorAscend, MaturityExperimental, []string{"libvruntime.so"}},
		{BackendPPULogical, VendorPPU, MaturityExperimental, []string{"hggc_quota.so", "hgml_dlsym_hook.so"}},
		{BackendHygonLogical, VendorHygon, MaturityExperimental, []string{"libamdhip64.so", "libhydmi.so", "libhydmi_mig.so", "librocm_smi64.so", "gpuworks-hygon-vdev-helper"}},
	}
	all := All()
	if len(all) != len(want) {
		t.Fatalf("registry length = %d, want %d", len(all), len(want))
	}
	for i, expected := range want {
		got := all[i]
		if got.ID() != expected.id || got.Metadata().Vendor != expected.vendor || got.Maturity() != expected.maturity || !reflect.DeepEqual(got.RequiredAssets(), expected.assets) {
			t.Fatalf("backend[%d] = id=%q vendor=%q maturity=%q assets=%v", i, got.ID(), got.Metadata().Vendor, got.Maturity(), got.RequiredAssets())
		}
		arch := ""
		if expected.id == BackendHygonLogical {
			arch = "Z100"
		}
		if !got.Matches(expected.vendor, arch) || !got.NeedsInstanceTeardown() {
			t.Fatalf("backend[%d] incomplete: %+v", i, got)
		}
		if lookedUp, ok := Lookup(expected.id); !ok || lookedUp.ID() != expected.id {
			t.Fatalf("Lookup(%q) = %v, %v", expected.id, lookedUp, ok)
		}
	}
	if _, ok := Lookup("made-up"); ok {
		t.Fatal("unknown backend unexpectedly found")
	}
}

func TestMatchProfileIncludesBuiltButUnverifiedCombinations(t *testing.T) {
	if _, err := MatchProfile(BackendAscendLogical, "cann", "8.5.1", "amd64"); err != nil {
		t.Fatal(err)
	}
	if _, err := MatchProfile(BackendAscendLogical, "cann", "9.0.0", "arm64"); err != nil {
		t.Fatal(err)
	}
	profile, err := MatchProfile(BackendAscendLogical, "cann", "9.0.0", "amd64")
	if err != nil || profile.Verified {
		t.Fatalf("CANN 9 amd64 profile = %+v, %v; want built but unverified", profile, err)
	}
	if _, warning := ProfileCompatibility(BackendAscendLogical, "cann", "", "arm64"); warning != "" {
		t.Fatalf("empty driver-only host version produced warning %q", warning)
	}
}

func TestMatchProfileUnknownBackendAndProfilelessBackend(t *testing.T) {
	if _, err := MatchProfile("made-up", "cann", "8.5", "amd64"); err == nil {
		t.Fatal("unknown backend unexpectedly matched a profile")
	}
	// NVIDIA is registered but built for no profiles; it must surface the same
	// unknown-backend error rather than silently matching.
	if _, err := MatchProfile(BackendNVIDIALogical, "cuda", "12", "amd64"); err == nil {
		t.Fatal("profileless backend unexpectedly matched a profile")
	}
}

func TestProfilesAreOwnedByTheirBackend(t *testing.T) {
	got := PartitionProfiles()
	if len(got) != 6 {
		t.Fatalf("PartitionProfiles() has %d rows, want 6 (4 Ascend + 1 PPU + 1 Hygon)", len(got))
	}
	for _, profile := range got {
		if profile.Backend == BackendAscendLogical && profile.Framework != "cann" {
			t.Fatalf("Ascend profile carries framework %q", profile.Framework)
		}
		if profile.Backend == BackendPPULogical && profile.Framework != "hggc" {
			t.Fatalf("PPU profile carries framework %q", profile.Framework)
		}
		if profile.Backend == BackendHygonLogical && (profile.Framework != "dtk" || profile.Verified) {
			t.Fatalf("Hygon profile must be advisory and unverified: %+v", profile)
		}
	}
}

func TestOptionalCapabilitiesAreBackendScoped(t *testing.T) {
	nvidia, _ := Lookup(BackendNVIDIALogical)
	ascend, _ := Lookup(BackendAscendLogical)
	ppu, _ := Lookup(BackendPPULogical)
	hygon, _ := Lookup(BackendHygonLogical)

	if !SkipsBehaviorProbe(nvidia) {
		t.Fatal("NVIDIA must skip the behavior probe")
	}
	if SkipsBehaviorProbe(ascend) || SkipsBehaviorProbe(ppu) || SkipsBehaviorProbe(hygon) {
		t.Fatal("Ascend, PPU, and Hygon must run the behavior probe")
	}

	if why := RuntimeConstraint(ppu, "docker"); why != "" {
		t.Fatalf("PPU under docker constrained: %q", why)
	}
	if why := RuntimeConstraint(ppu, "udocker"); why == "" {
		t.Fatal("PPU under udocker must report a runtime constraint")
	}
	if why := RuntimeConstraint(hygon, "docker"); why != "" {
		t.Fatalf("Hygon under docker constrained: %q", why)
	}
	for _, driver := range []string{"", "udocker", "nerdctl"} {
		if why := RuntimeConstraint(hygon, driver); why == "" {
			t.Errorf("Hygon under %q must report a runtime constraint", driver)
		}
	}
	if why := RuntimeConstraint(ascend, "udocker"); why != "" || RuntimeConstraint(nvidia, "nerdctl") != "" {
		t.Fatal("Ascend and NVIDIA must be runtime-unconstrained")
	}

	if len(HostRequirementNotes(ascend)) == 0 {
		t.Fatal("Ascend must surface host requirement notes")
	}
	if len(HostRequirementNotes(nvidia)) != 0 || len(HostRequirementNotes(ppu)) != 0 {
		t.Fatal("NVIDIA and PPU must not surface host requirement notes")
	}
}

func TestSupportsFrameworkFollowsBackendProfiles(t *testing.T) {
	ascend, _ := Lookup(BackendAscendLogical)
	ppu, _ := Lookup(BackendPPULogical)
	nvidia, _ := Lookup(BackendNVIDIALogical)
	hygon, _ := Lookup(BackendHygonLogical)
	if !SupportsFramework(ascend, "CANN") || SupportsFramework(ascend, "hggc") {
		t.Fatal("Ascend framework support mismatch")
	}
	if !SupportsFramework(ppu, "hggc") || SupportsFramework(ppu, "cann") {
		t.Fatal("PPU framework support mismatch")
	}
	if SupportsFramework(nvidia, "cuda") {
		t.Fatal("NVIDIA has no profiles and must not claim framework support")
	}
	if !SupportsFramework(hygon, "DTK") || SupportsFramework(hygon, "rocm") {
		t.Fatal("Hygon framework support mismatch")
	}
}

func TestHygonBackendMatchesOnlyKnownZ100(t *testing.T) {
	hygon, ok := Lookup(BackendHygonLogical)
	if !ok {
		t.Fatal("Hygon backend is not registered")
	}
	for _, tc := range []struct {
		vendor, arch string
		want         bool
	}{
		{VendorHygon, "Z100", true},
		{" DCU ", " z100 ", true},
		{VendorHygon, "", false},
		{VendorHygon, "UNKNOWN_HYGON_SKU", false},
		{VendorHygon, "K100AI", false},
		{VendorAMD, "Z100", false},
	} {
		if got := hygon.Matches(tc.vendor, tc.arch); got != tc.want {
			t.Errorf("Matches(%q, %q) = %v, want %v", tc.vendor, tc.arch, got, tc.want)
		}
	}
	profile, err := MatchProfile(BackendHygonLogical, "dtk", "26.04.1", "amd64")
	if err != nil || profile.Verified {
		t.Fatalf("Hygon Z100 profile = %+v, %v; want unverified advisory profile", profile, err)
	}
}
