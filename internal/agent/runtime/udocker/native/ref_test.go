package native

import (
	"strings"
	"testing"
)

func TestNewClientUsesRequestedPlatform(t *testing.T) {
	for _, tc := range []struct{ arch string }{{"amd64"}, {"arm64"}} {
		t.Run(tc.arch, func(t *testing.T) {
			c := NewClientForPlatform("linux", tc.arch)
			if c.Platform.OS != "linux" || c.Platform.Architecture != tc.arch {
				t.Fatalf("platform = %+v, want linux/%s", c.Platform, tc.arch)
			}
		})
	}
}

func TestParseRefSplitsLocalAndRemoteNames(t *testing.T) {
	tests := []struct {
		image    string
		registry string
		remote   string
		local    string
		tag      string
	}{
		// A bare Docker Hub name keeps its short form locally (that is what
		// `udocker images` prints) but needs "library/" on the wire.
		{"ubuntu", DockerHub, "library/ubuntu", "ubuntu", "latest"},
		{"ubuntu:24.04", DockerHub, "library/ubuntu", "ubuntu", "24.04"},
		{"vllm/vllm-openai:latest", DockerHub, "vllm/vllm-openai", "vllm/vllm-openai", "latest"},
		{"quay.io/gpustack/runner:cuda13.0-vllm0.25.1", "quay.io", "gpustack/runner", "quay.io/gpustack/runner", "cuda13.0-vllm0.25.1"},
		// A registry with a port must not be mistaken for a tag.
		{"registry:5000/team/img:v1", "registry:5000", "team/img", "registry:5000/team/img", "v1"},
		{"localhost:5000/img", "localhost:5000", "img", "localhost:5000/img", "latest"},
		{"swr.cn-south-1.myhuaweicloud.com/gpustack/runner:v1", "swr.cn-south-1.myhuaweicloud.com", "gpustack/runner", "swr.cn-south-1.myhuaweicloud.com/gpustack/runner", "v1"},
		{"library/ubuntu", DockerHub, "library/ubuntu", "library/ubuntu", "latest"},
		{"ubuntu@sha256:abc", DockerHub, "library/ubuntu", "ubuntu", "sha256:abc"},
	}
	for _, tc := range tests {
		got := ParseRef(tc.image)
		if got.Registry != tc.registry || got.Remote != tc.remote || got.Local != tc.local || got.Tag != tc.tag {
			t.Errorf("ParseRef(%q) = registry=%q remote=%q local=%q tag=%q, want registry=%q remote=%q local=%q tag=%q",
				tc.image, got.Registry, got.Remote, got.Local, got.Tag, tc.registry, tc.remote, tc.local, tc.tag)
		}
	}
}

func TestCleanEntryPathRejectsEscapes(t *testing.T) {
	for _, name := range []string{"../etc/passwd", "/../etc/passwd", "foo/../../etc/passwd"} {
		if _, err := cleanEntryPath(name); err == nil {
			t.Errorf("cleanEntryPath(%q) accepted a path escaping the destination", name)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"usr/bin/env", "usr/bin/env"},
		{"./usr/bin/env", "usr/bin/env"},
		{"/usr/bin/env", "usr/bin/env"},
		{"usr//bin/./env", "usr/bin/env"},
		{"usr/lib/../bin/env", "usr/bin/env"},
	} {
		got, err := cleanEntryPath(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("cleanEntryPath(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestParseWhiteoutClassifiesMarkers(t *testing.T) {
	if mark, ok := parseWhiteout("usr/lib/.wh.libfoo.so"); !ok || mark.opaque || mark.target != "usr/lib/libfoo.so" {
		t.Errorf("plain whiteout = %+v ok=%v", mark, ok)
	}
	if mark, ok := parseWhiteout("var/cache/.wh..wh..opq"); !ok || !mark.opaque || mark.target != "var/cache" {
		t.Errorf("opaque whiteout = %+v ok=%v", mark, ok)
	}
	if mark, ok := parseWhiteout(".wh..wh..opq"); !ok || !mark.opaque || mark.target != "" {
		t.Errorf("root opaque whiteout = %+v ok=%v", mark, ok)
	}
	if _, ok := parseWhiteout("usr/lib/libfoo.so"); ok {
		t.Error("ordinary path classified as a whiteout")
	}
}

func TestExcludedMatchesUdockerCreateExclusions(t *testing.T) {
	for _, name := range []string{"dev/null", "etc/udev/devices/foo"} {
		if !excluded(name) {
			t.Errorf("excluded(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"etc/passwd", "usr/bin/dev", "development/x"} {
		if excluded(name) {
			t.Errorf("excluded(%q) = true, want false", name)
		}
	}
}

// FullName is what containerd image stores index an image by; Docker 29
// cannot find an image loaded under a short name.
func TestRefFullNameIsDockerQualified(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for image, want := range map[string]string{
		"alpine":                         "docker.io/library/alpine:latest",
		"alpine:3.20":                    "docker.io/library/alpine:3.20",
		"docker.io/alpine:3.20":          "docker.io/library/alpine:3.20",
		"vllm/vllm-openai:v0.25.1":       "docker.io/vllm/vllm-openai:v0.25.1",
		"quay.io/gpustack/runner:v1":     "quay.io/gpustack/runner:v1",
		"localhost:5000/team/app:v1":     "localhost:5000/team/app:v1",
		"registry.example/app@" + digest: "registry.example/app@" + digest,
	} {
		if got := ParseRef(image).FullName(); got != want {
			t.Errorf("FullName(%s) = %s, want %s", image, got, want)
		}
	}
}
