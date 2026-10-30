package deploy

import "testing"

func TestExplicitImageCannotOverrideKnownPlatformMismatch(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		got := ResolveImageCatalog(ImageTarget{Engine: "vllm", PlatformArch: "amd64", Override: "custom/arm:v1", TrustedOverride: trusted}, []ImageCandidate{{Engine: "vllm", Arch: "arm64", ImageRef: "custom/arm:v1", Enabled: true}})
		if !got.Blocking {
			t.Fatalf("known incompatible platform accepted (trusted=%v): %+v", trusted, got)
		}
	}
}

func TestImageExecutionPlatformConstraints(t *testing.T) {
	for _, tc := range []struct {
		name, framework, candidateArch, targetOS, targetArch string
		blocked                                              bool
	}{
		{"darwin package on matching arm linux", "metal", "arm64", "linux", "arm64", true},
		{"linux image on mac host", "cuda", "arm64", "darwin", "arm64", true},
		{"mac package on mac host", "metal", "arm64", "darwin", "aarch64", false},
		{"linux x86 alias", "cuda", "x86_64", "linux", "amd64", false},
		{"linux multi", "cuda", "multi", "linux", "arm64", false},
		{"unknown private platform", "", "", "linux", "amd64", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := ImageCandidate{Engine: "vllm", Framework: tc.framework, Arch: tc.candidateArch, ImageRef: "test/image:v1", Enabled: true}
			got := ResolveImageCatalog(ImageTarget{Engine: "vllm", PlatformArch: tc.targetArch, PlatformOS: tc.targetOS, Override: candidate.ImageRef}, []ImageCandidate{candidate})
			if got.Blocking != tc.blocked {
				t.Fatalf("resolution: %+v, want blocked=%v", got, tc.blocked)
			}
		})
	}
}
