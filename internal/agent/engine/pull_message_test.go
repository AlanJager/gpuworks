package engine

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A replica that pulls its image keeps saying which source it pulls from: the
// byte counters carry the progress, a runtime line follows the source only
// when there are none, and extraction speaks for itself.
func TestPullMessageNamesTheSource(t *testing.T) {
	const ref = "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/runner:v1"
	for name, tc := range map[string]struct {
		snapshot imagepull.Snapshot
		want     string
	}{
		"with byte counters": {imagepull.Snapshot{Phase: proto.PhaseImagePull, Message: "abc: Downloading 1MB/2MB", Done: 1 << 20, Total: 2 << 20}, "pulling " + ref},
		"without counters":   {imagepull.Snapshot{Phase: proto.PhaseImagePull, Message: "abc: Pulling fs layer"}, "pulling " + ref + ": abc: Pulling fs layer"},
		"nothing printed":    {imagepull.Snapshot{}, "pulling " + ref},
		"extracting":         {imagepull.Snapshot{Phase: proto.PhaseImageExtract, Message: "abc: Extracting 1MB/2MB", Total: 2 << 20}, "abc: Extracting 1MB/2MB"},
	} {
		if got := pullMessage(ref, tc.snapshot); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
