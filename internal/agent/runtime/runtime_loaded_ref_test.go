package runtime

import "testing"

func TestParseLoadedImageRefReadsWhatTheRuntimeActuallyImported(t *testing.T) {
	for name, tc := range map[string]struct {
		output string
		want   string
	}{
		"docker": {
			output: "Loaded image: gpustack/runner:cuda13.0-vllm0.25.1\n",
			want:   "gpustack/runner:cuda13.0-vllm0.25.1",
		},
		"docker with progress noise": {
			output: "e2eb06d8af82: Loading layer [====>]  65.5MB/65.5MB\nLoaded image: registry.example/team/app:v2\n",
			want:   "registry.example/team/app:v2",
		},
		"truncated docker image ID": {
			output: "Loaded image ID: sha256:1f2c3d4e5f60\n",
			want:   "",
		},
		"nerdctl": {
			output: "unpacking gpustack/runner:cuda13.0-vllm0.25.1 (sha256:aabbccdd)...\ndone\n",
			want:   "gpustack/runner:cuda13.0-vllm0.25.1",
		},
		"nerdctl without digest suffix": {
			output: "unpacking registry.example/team/app:v2\n",
			want:   "registry.example/team/app:v2",
		},
		"crlf line endings": {
			output: "Loaded image: registry.example/team/app:v2\r\n",
			want:   "registry.example/team/app:v2",
		},
		"last entry of a multi-image archive wins": {
			output: "Loaded image: a/one:v1\nLoaded image: b/two:v2\n",
			want:   "b/two:v2",
		},
		"nothing recognisable": {
			output: "error: no such file\n",
			want:   "",
		},
		"empty": {output: "", want: ""},
	} {
		if got := ParseLoadedImageRef(tc.output); got != tc.want {
			t.Errorf("%s: ParseLoadedImageRef(%q) = %q, want %q", name, tc.output, got, tc.want)
		}
	}
}

func TestParseLoadedImageRefForPrefersTheRequestedEntry(t *testing.T) {
	const id = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const other = "sha256:1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const idLine = "Loaded image ID: " + id + "\n"
	for _, tc := range []struct {
		name, output, image, want string
	}{
		{"docker first entry", "Loaded image: a/one:v1\nLoaded image: b/two:v2\n", "a/one:v1", "a/one:v1"},
		{"nerdctl first entry", "unpacking a/one:v1 (sha256:aaaa)...\nunpacking b/two:v2 (sha256:bbbb)...\n", "a/one:v1", "a/one:v1"},
		{"fully qualified Hub output", "Loaded image: docker.io/library/alpine:latest\nLoaded image: b/two:v2\n", "alpine", "alpine"},
		{"short Hub output", "Loaded image: alpine:latest\nLoaded image: b/two:v2\n", "docker.io/library/alpine:latest", "docker.io/library/alpine:latest"},
		{"Hub host alias", "Loaded image: index.docker.io/team/one:v1\nLoaded image: b/two:v2\n", "docker.io/team/one:v1", "docker.io/team/one:v1"},
		{"registry endpoint is a separate stored name", "Loaded image: registry-1.docker.io/library/alpine:latest\nLoaded image: b/two:v2\n", "docker.io/alpine:latest", "b/two:v2"},
		{"registry default tag", "Loaded image: localhost:5000/team/one:latest\nLoaded image: b/two:v2\n", "localhost:5000/team/one", "localhost:5000/team/one"},
		{"different registry is not equivalent", "Loaded image: mirror.example/team/one:v1\nLoaded image: b/two:v2\n", "team/one:v1", "b/two:v2"},
		{"different tag is not equivalent", "Loaded image: a/one:v2\nLoaded image: b/two:v2\n", "a/one:v1", "b/two:v2"},
		{"missing requested entry keeps fallback", "Loaded image: a/one:v1\nLoaded image: b/two:v2\n", "catalog/runner:latest", "b/two:v2"},
		{"single untagged image", idLine, "a/one:v1", id},
		{"same ID repeated", idLine + idLine, "a/one:v1", id},
		{"multiple distinct IDs", idLine + "Loaded image ID: " + other, "a/one:v1", ""},
		{"malformed ID mixed with valid ID", "Loaded image ID: invalid\n" + idLine, "a/one:v1", ""},
		{"mixed identities with no requested ref", idLine + "Loaded image: b/two:v2", "a/one:v1", ""},
		{"requested ref among mixed identities", idLine + "Loaded image: a/one:v1", "a/one:v1", "a/one:v1"},
		{"unknown output stays unknown", "Loaded image ID: sha256:abcdef\n", "a/one:v1", ""},
		{"no preference keeps last entry", "Loaded image: a/one:v1\nLoaded image: b/two:v2\n", "", "b/two:v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseLoadedImageRefFor(tc.output, tc.image); got != tc.want {
				t.Fatalf("selected %q, want %q", got, tc.want)
			}
		})
	}
}
