//go:build cloudgpu && partition

package l4

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A pool member without docker/nerdctl runs the same acceptance case under
// runveil (see ascendPoolRuntimeProbe). Its fixture is an already-loaded
// private udocker store: the host's ascendRunveilCachePath. Linking only
// repos/layers into the temporary agent's state avoids importing old
// containers, credentials, runtime assets or diagnostic hooks, and costs no
// disk for the multi-gigabyte layers. The run root shares the cache's
// filesystem (ascendPoolRemoteRoot), so the links never cross devices.
func stageAscendRunveilCache(t *testing.T, node *ascendPoolNode) {
	t.Helper()
	source := ascendRunveilCachePath()
	if err := validateAscendRunveilCache(source); err != nil {
		t.Fatal(err)
	}
	destination := node.remoteDir + "/state/udocker"
	command := fmt.Sprintf("mkdir -p %s; cp -al %s/repos %s/layers %s/", shellQuoteL4(destination), shellQuoteL4(source), shellQuoteL4(source), shellQuoteL4(destination))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if out, err := partitionSSHCommand(ctx, node.candidate.host, node.candidate.jump, command).CombinedOutput(); err != nil {
		t.Fatalf("stage runveil fixture: %v: %s", err, out)
	}
}

func assertAscendRunveilQuota(t *testing.T, node *ascendPoolNode, containerID string) {
	t.Helper()
	script := `import torch,torch_npu
free,total=torch.npu.mem_get_info(0)
assert total==8*1024**3, total
try:
 x=torch.empty(9*1024**3,dtype=torch.uint8,device='npu')
except RuntimeError as e:
 print('platform_over_quota_denied_runveil', total, str(e), flush=True)
else:
 raise AssertionError('9 GiB allocation exceeded 8 GiB quota')
`
	command := fmt.Sprintf("%s/gpuworks-agent runveil --state %s/state exec %s -- python3 -c %s", shellQuoteL4(node.remoteDir), shellQuoteL4(node.remoteDir), shellQuoteL4(containerID), shellQuoteL4(script))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	out, err := partitionSSHCommand(ctx, node.candidate.host, node.candidate.jump, command).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "platform_over_quota_denied_runveil") {
		t.Fatalf("runveil quota on platform container: %v\n%s", err, out)
	}
	archivePartitionEvidence(t, "runveil-quota-"+containerID+".txt", string(out))
}
