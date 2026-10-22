package deploy

import "testing"

func TestImageEngineVersion(t *testing.T) {
	for _, tc := range []struct {
		engine, ref, catalog, want string
	}{
		{"vllm", "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2", "0.30.0", "0.30.0"},
		{"vllm", "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2", "", "0.30.0"},
		{"vllm", "gpuworks/gpuworks-runner:cann9.1-910b-vllm0.26.0rc1-r2", "", "0.26.0rc1"},
		{"vllm", "swr.cn-south-1.myhuaweicloud.com/gpustack/runner:cuda12.9-vllm0.25.1", "", "0.25.1"},
		{"vllm", "vllm/vllm-openai:v0.30.0", "", "0.30.0"},
		{"vllm", "image.sourcefind.cn:5000/dcu/admin/base/vllm:0.9.2-ubuntu22.04-dtk25.04.2-py3.10", "", "0.9.2"},
		{"vllm", "registry.example:5000/vllm/vllm-openai:latest", "", ""},
		// Only other components' versions in the tag say nothing about vLLM.
		{"vllm", "registry.example/team/serving:cuda12.9-py3.12", "", ""},
		{"vllm", "lmsysorg/sglang:v0.5.2-cu126", "", ""},
		{"sglang", "lmsysorg/sglang:v0.5.2-cu126", "", "0.5.2"},
		{"vllm", "vllm/vllm-openai@sha256:0123", "", ""},
		{"vllm", "vllm/vllm-openai:v0.30.0@sha256:0123", "", "0.30.0"},
		{"vllm", "", "", ""},
	} {
		if got := ImageEngineVersion(tc.engine, tc.ref, tc.catalog); got != tc.want {
			t.Errorf("ImageEngineVersion(%q, %q, %q) = %q, want %q", tc.engine, tc.ref, tc.catalog, got, tc.want)
		}
	}
}
