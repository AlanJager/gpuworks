//go:build cloudgpu && partition

package l4

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

type giteePartitionMatrixEntry struct {
	name, zone, model, image, frameworkVersion string
	// archs carries every architecture the profile may boot as, in the
	// accelerator.NormalizeArch spelling (x86_64, arm64); the observed
	// node must match one of them. Gitee publishes per-model image lists but
	// no architecture field, and fallback models share images across arches.
	archs                                         []string
	wantRuntime, wantProbeStatus, wantProbeDetail string
	wantReady                                     bool
}

func TestGiteeAscendPartitionCapability(t *testing.T) {
	secrets := skipIfNoSecrets(t, "GITEE_API_TOKEN")
	runID, err := cloudProviderRunID()
	if err != nil {
		t.Fatal(err)
	}
	// Every profile is ordered coverage with an availability escape: Gitee
	// keeps sold-out models listed in /compute/specs and only the create call
	// reveals host supply, so a profile whose create returns "没有找到符合条件
	// 的可用主机" is skipped this run and the next profile carries the signal.
	// 910B2C was rented out across 2026-10-02..03 while 910B2 stayed listable.
	matrix := []giteePartitionMatrixEntry{
		{
			name: "CANN8_5_x86_64_910B2C", zone: "gitee", model: "Ascend 910B2C", archs: []string{"x86_64"}, frameworkVersion: "8.5",
			image:       "easzlab.io.local:5000/ascend/torch-npu:2.9.0-CANN-8.5.1-py311-openeuler24.03",
			wantRuntime: "udocker", wantProbeStatus: proto.PartitionProbeFailed,
			wantProbeDetail: "only root user on physical machine",
		},
		{
			// The CANN9 fallback profile: same outer-container boundary, same
			// unprivileged vCANN probe verdict expected under udocker/PRoot.
			// Gitee offers this image for both 910B2C and 910B2, and the 910B2
			// arch is not advertised, so both arches are accepted here and the
			// observed one is recorded into the evidence file.
			name: "CANN9_910B2", zone: "gitee", model: "Ascend 910B2", archs: []string{"x86_64", "arm64"}, frameworkVersion: "9",
			image:           "easzlab.io.local:5000/metax/torch-npu:2.10.0-910b-openeuler24.03-py3.11",
			wantRuntime:     "udocker",
			wantProbeStatus: proto.PartitionProbeFailed,
			wantProbeDetail: "only root user on physical machine",
		},
		// x86_64/CANN9 and arm64/CANN9 serving coverage is asserted by the two
		// full deploy+chat cases. Gitee lists a CANN8.5 arm64 image but rejects
		// it as an outer rental with "镜像和主机不匹配"; the inner CANN8.5/PRoot
		// limitation is recorded separately rather than mislabeled as passing
		// coverage.
	}
	rented := false
	for _, entry := range matrix {
		entry := entry
		t.Run(entry.name, func(t *testing.T) {
			provider, err := cloudprovider.NewGitee(secrets["GITEE_API_TOKEN"], entry.zone, entry.image, runID+"-"+strings.ToLower(entry.name))
			if err != nil {
				t.Fatal(err)
			}
			probeCtx, probeCancel := context.WithTimeout(t.Context(), 3*time.Minute)
			instance, rentErr := provider.Rent(probeCtx, cloudprovider.GPUSpec{GPUModel: entry.model, GPUCount: 1, MinVRAMMB: 32 << 10})
			probeCancel()
			if rentErr != nil {
				if isCloudCapacityError(rentErr) {
					t.Skipf("gitee has no %s host right now, capability profile untested this run: %v", entry.model, rentErr)
				}
				t.Fatal(rentErr)
			}
			rented = true
			env := setupCloudNodeWithInstance(t, testenv.New(t), provider, cloudprovider.GPUSpec{GPUModel: entry.model, GPUCount: 1, MinVRAMMB: 32 << 10}, instance)
			requireOnlineNode(t, env)
			node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
			if err != nil {
				t.Fatal(err)
			}
			archOK := false
			for _, arch := range entry.archs {
				if accelerator.NormalizeArch(node.Arch) == arch {
					archOK = true
					break
				}
			}
			if !archOK || !strings.HasPrefix(node.FrameworkVersion, entry.frameworkVersion) {
				t.Fatalf("observed profile arch=%q framework=%q/%q, want one of %v CANN %s.x", node.Arch, node.Framework, node.FrameworkVersion, entry.archs, entry.frameworkVersion)
			}
			if len(node.GPUs) < 1 {
				t.Fatal("rented Ascend node reported no GPUs")
			}
			capabilities := partitioncap.ForGPU(node, &node.GPUs[0])
			var probeResults []proto.PartitionProbeResult
			if err := json.Unmarshal([]byte(node.PartitionProbes), &probeResults); err != nil {
				t.Fatalf("decode partition probes %q: %v", node.PartitionProbes, err)
			}
			probeStatus, probeDetail := "", ""
			for _, result := range probeResults {
				if result.Backend == accelerator.BackendAscendLogical {
					probeStatus, probeDetail = result.Status, result.Detail
				}
			}
			wantStatus := partitioncap.StatusUnavailable
			if entry.wantReady {
				wantStatus = partitioncap.StatusReady
			}
			if len(capabilities) != 1 || capabilities[0].Backend != accelerator.BackendAscendLogical || capabilities[0].Status != wantStatus || entry.wantReady && probeStatus != proto.PartitionProbePassed || !entry.wantReady && probeStatus != entry.wantProbeStatus || !strings.Contains(probeDetail, entry.wantProbeDetail) || node.RuntimeDriver != entry.wantRuntime || !strings.Contains(node.PartitionAssets, "libvruntime.so") {
				diagnostic, diagnosticErr := runCloudCommand(t.Context(), env.Instance, `set +e
printf 'arch='; uname -m
printf '\nnpu-smi-info\n'; timeout 20s npu-smi info 2>&1
printf '\ndevice-share-card-0\n'; timeout 10s npu-smi info -t device-share -i 0 -c 0 2>&1
printf '\ndevice-share-card-1\n'; timeout 10s npu-smi info -t device-share -i 1 -c 0 2>&1
printf '\nassets\n'; find /opt/gpuworks /tmp/gpuworks -type f -path '*dgpu-runtime*' -printf '%p %s bytes\n' 2>/dev/null | sort
printf 'disk\n'; df -h / /opt/gpuworks 2>/dev/null
printf 'agent-log\n'; tail -n 100 /opt/gpuworks/agent.log /tmp/gpuworks/agent.log 2>&1 | sed -E 's/(node-token[= ]+)[^ ]+/\1[redacted]/g'`)
				archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+"-failure.txt", diagnostic)
				t.Fatalf("Ascend capability=%+v raw_assets=%q diagnostic_err=%v\n%s", capabilities, node.PartitionAssets, diagnosticErr, diagnostic)
			}
			archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", fmt.Sprintf("instance=ledger-owned-and-destroyed\nmodel=%s\narch=%s\nframework=%s %s\nruntime=%s\ngpu=%s %s\nprobe=%s raw=%s\ncapability=%+v\n", entry.model, node.Arch, node.Framework, node.FrameworkVersion, node.RuntimeDriver, node.GPUs[0].Name, node.GPUs[0].ArchID, probeStatus, node.PartitionProbes, capabilities[0]))

			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := env.cleanup(cleanupCtx); err != nil {
				t.Fatalf("destroy Gitee instance %s: %v", env.Instance.InstanceID, err)
			}
			leaked, err := provider.ListLeakedForRunID(cleanupCtx, provider.RunID())
			if err != nil || len(leaked) != 0 {
				t.Fatalf("Gitee cleanup run=%s leaked=%v err=%v", provider.RunID(), leaked, err)
			}
		})
	}
	if !rented {
		skipOrFailExternal(t, "no Gitee Ascend profile rentable: every candidate create reported no available host")
	}
}
