package pullref

import (
	"testing"
)

func TestNormalizeRefAndPrefixFollowRegistryResolution(t *testing.T) {
	for _, tc := range []struct{ in, wantRef, wantPrefix string }{
		{"nginx", "docker.io/library/nginx", "docker.io/nginx"},
		{"vllm/vllm-openai", "docker.io/vllm/vllm-openai", "docker.io/vllm/vllm-openai"},
		{"docker.io/library/nginx", "docker.io/library/nginx", "docker.io/library/nginx"},
		{"quay.io/gpustack/runner", "quay.io/gpustack/runner", "quay.io/gpustack/runner"},
		{"registry.example:5000/team/app", "registry.example:5000/team/app", "registry.example:5000/team/app"},
		{"localhost:5000/app", "localhost:5000/app", "localhost:5000/app"},
	} {
		if got := NormalizeRef(tc.in); got != tc.wantRef {
			t.Errorf("NormalizeRef(%q) = %q, want %q", tc.in, got, tc.wantRef)
		}
		if got := NormalizePrefix(tc.in); got != tc.wantPrefix {
			t.Errorf("NormalizePrefix(%q) = %q, want %q", tc.in, got, tc.wantPrefix)
		}
	}
	if got := NormalizeRef(""); got != "" {
		t.Errorf("NormalizeRef(empty) = %q", got)
	}
	// Operators habitually write rule prefixes with a trailing slash; that
	// spelling must name the same namespace as the bare one.
	if got := NormalizePrefix("gpustack/"); got != "docker.io/gpustack" {
		t.Errorf("NormalizePrefix(trailing slash) = %q", got)
	}
	// A prefix of only separators or whitespace carries no namespace and must
	// normalize to the empty string, which Rewrite then skips entirely.
	for _, blank := range []string{"", "   ", "/", " / "} {
		if got := NormalizePrefix(blank); got != "" {
			t.Errorf("NormalizePrefix(%q) = %q, want empty", blank, got)
		}
	}
}

func TestSourceLabelsIdentifyKnownRegistries(t *testing.T) {
	canonical := "gpustack/runner:v1"
	for _, tc := range []struct{ ref, want string }{
		{canonical, SourceDirect},
		{"quay.io/gpustack/runner:v1", SourceQuay},
		{"swr.cn-south-1.myhuaweicloud.com/gpustack/runner:v1", SourceHuaweiCloud},
		{"swr.example/gpustack/runner:v1", SourceHuaweiCloud},
		{"mirror.example/gpustack/runner:v1", SourceMirror},
	} {
		if got := Source(tc.ref, canonical); got != tc.want {
			t.Errorf("Source(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

// An image archive URL lives in the same registry_sources list as registry
// refs, because that list is what the node table and the deployment page read
// to offer acquisition choices. Telling the two apart is this one predicate:
// anything that gets it wrong ends up passing a URL to "docker pull".
func TestIsArchiveURLSeparatesArchivesFromRegistryRefs(t *testing.T) {
	for _, entry := range []string{
		"https://containerimages.zstack.ai/gpustack_runner_cuda13.0-vllm0.25.1.tar",
		"http://10.0.0.5/images/runner.tar",
		"HTTPS://Containerimages.ZStack.ai/runner.tar",
		"  https://containerimages.zstack.ai/runner.tar  ",
		"http://mirror.internal:8081/images/runner.tar",
	} {
		if !IsArchiveURL(entry) {
			t.Errorf("IsArchiveURL(%q) = false, want true", entry)
		}
	}
	for _, entry := range []string{
		"",
		"   ",
		"quay.io/gpustack/runner:v1",
		"gpustack/runner:v1",
		"localhost:5000/runner:v1",
		"registry.example.com/team/runner:v1",
		// A host that merely starts with the letters is still a registry ref.
		"https.example.com/runner:v1",
		"httpsomething/runner:v1",
		// A scheme this platform does not fetch archives over is not an
		// archive URL; it is simply not a valid ref either.
		"ftp://mirror/runner.tar",
	} {
		if IsArchiveURL(entry) {
			t.Errorf("IsArchiveURL(%q) = true, want false", entry)
		}
	}
}
