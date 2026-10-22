package proto

import "testing"

func TestAgentBinaryName(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "gpuworks-agent-linux-amd64"},
		{"darwin", "arm64", "gpuworks-agent-darwin-arm64"},
		// An agent that predates reporting its OS ran on Linux.
		{"", "arm64", "gpuworks-agent-linux-arm64"},
	} {
		if got := AgentBinaryName(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("AgentBinaryName(%q, %q) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}
