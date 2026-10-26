//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

// vmBackend is what one VM provider contributes to the scenarios the
// providers share (single-VM tensor parallel + dGPU, one-VM two-logical-node
// and two-VM Docker distributed inference): the rented machines, how a card
// shape is picked, and where the Docker image and the model come from. The
// scenario bodies are provider-neutral and take a backend, so a new provider
// adds a constructor here and thin Test functions, not a copy of the cases.
type vmBackend struct {
	// Name labels logs and served model names ("compshare", "aliyun").
	Name     string
	Provider cloudprovider.CloudProvider
	// EachShape calls try for each card type and GPU count with at least cards
	// GPUs that looks rentable, until try returns true (hosted selects the
	// shapes the multi-VM control-plane cases use). It returns why every shape
	// was passed over when none worked.
	EachShape func(t *testing.T, hosted bool, cards, minVRAMMB int, try func(model string, count int) bool) []string
	// DiskGB is the system disk of every VM.
	DiskGB int
	// ModelEndpoint is an optional model mirror site rule.
	ModelEndpoint string
	// PullMirror is the replacement endpoint list of the gpustack/ image source
	// rule; empty sets no rule (the image is already on the VM).
	PullMirror []string
	// Image is the vLLM runner single-VM cases deploy; DistributedImage the one
	// multi-node cases do.
	Image, DistributedImage string
	// Engine is the vLLM image and extra engine arguments a card type runs, and
	// false for a card no image of ours can run (CompShare: V100S takes the
	// 1Cat-vLLM Volta image, P40 is skipped). EnsureEngineImage makes that image
	// present on the VM before the expensive part of a single-VM case.
	Engine            func(gpuModel string) (image, extraArgs string, ok bool)
	EnsureEngineImage func(t *testing.T, env *cloudTestEnv, gpuModel string)
	// HostedRoot is the directory the hosted control plane and agents keep their
	// state in; empty means /tmp (a non-root login cannot write elsewhere).
	HostedRoot string
	// UdockerDiskGB is the system disk of the VMs of the udocker case, which
	// keeps the runner image twice (layers, then an unpacked container: 30+ GiB
	// each way) on the VM itself; zero means DiskGB. UdockerOnDemand rents that
	// case on-demand because it runs over an hour, long enough that a spot
	// reclaim is likely.
	UdockerDiskGB   int
	UdockerOnDemand bool
	// DockerTestImage is the image the Docker NCCL probe starts; empty means
	// DistributedImage.
	DockerTestImage string
	// PreloadedModelPath resolves the model directory present on every VM, or
	// skips/fails the case when the provider has none configured.
	PreloadedModelPath func(t *testing.T) string
	// EnsureDocker makes Docker 29 with the NVIDIA runtime usable on every VM
	// (CompShare installs it; Aliyun's golden image already carries it, so the
	// backend only verifies). EnsureImage makes the runner image present
	// without a registry pull.
	EnsureDocker func(t *testing.T, envs []*cloudTestEnv)
	EnsureImage  func(t *testing.T, envs []*cloudTestEnv)
}

// withEngine fills the engine, image and extra arguments a card type needs
// into a deployment body.
func (b *vmBackend) withEngine(body map[string]any, gpuModel string) map[string]any {
	image, args, ok := b.Engine(gpuModel)
	if !ok {
		panic(fmt.Sprintf("no engine image runs on %s %s", b.Name, gpuModel))
	}
	body["engine"] = "vllm"
	body["image"] = image
	if args != "" {
		body["extra_args"] = args
	}
	return body
}

func (b *vmBackend) hostedRoot() string {
	if b.HostedRoot != "" {
		return strings.TrimRight(b.HostedRoot, "/")
	}
	return "/tmp"
}

func (b *vmBackend) dockerTestImage() string {
	if b.DockerTestImage != "" {
		return b.DockerTestImage
	}
	return b.DistributedImage
}

// servedName builds a model name unique to the provider and scenario.
func (b *vmBackend) servedName(scenario string) string {
	return "qwen2-" + b.Name + "-" + scenario
}

// compshareAcceptanceDiskGB: the externally maintained golden image currently
// requires a system disk larger than 100 GiB. Every CompShare shape stays
// compatible with both that image and the ordinary fallback image.
const compshareAcceptanceDiskGB = 160

func compshareBackend(t *testing.T) *vmBackend {
	t.Helper()
	p := compshareProvider(t)
	return &vmBackend{
		Name: "compshare", Provider: p, DiskGB: 160,
		EachShape: func(t *testing.T, hosted bool, cards, minVRAMMB int, try func(string, int) bool) []string {
			// Single-VM cases start an engine on the card, so only types an engine
			// image exists for (see compshareEngineImage).
			models := compshareRunnableGPUOrder()
			if hosted {
				models = hostedCompshareGPUModels
			}
			return eachCompshareShape(t, p, models, cards, minVRAMMB, 160, try)
		},
		ModelEndpoint: compshareModelEndpoint(),
		PullMirror:    gpustackMirrorEndpoints,
		Image:         compshareTestImage(), DistributedImage: compshareDistributedTestImage(),
		DockerTestImage: strings.TrimSpace(os.Getenv("COMPSHARE_DOCKER_TEST_IMAGE")),
		PreloadedModelPath: func(t *testing.T) string {
			path := strings.TrimSpace(os.Getenv("COMPSHARE_PRELOADED_MODEL_PATH"))
			if path == "" {
				skipOrFailExternal(t, "COMPSHARE_PRELOADED_MODEL_PATH is required for CompShare deployment cases; use TestCompshareModelScopePublicDownload for public download acceptance")
			}
			return path
		},
		Engine: func(gpuModel string) (string, string, bool) {
			image, ok := compshareEngineImage(gpuModel)
			return image, compshareEngineExtraArgs(gpuModel), ok
		},
		EnsureEngineImage: func(t *testing.T, env *cloudTestEnv, gpuModel string) { prepareCompshareEngineImage(t, env, gpuModel) },
		EnsureDocker:      installCompshareDocker29OnNodes,
		EnsureImage:       prepareCompshareDockerImageCache,
	}
}

// countGPUCards counts nvidia-smi rows naming the card. A provider's GPU type
// (CompShare's 4090_48G) is not what nvidia-smi prints (NVIDIA GeForce RTX
// 4090), so callers pass the display name and spacing differences are ignored.
func countGPUCards(nvidiaSMI, displayName string) int {
	count := 0
	for _, line := range strings.Split(nvidiaSMI, "\n") {
		name, _, _ := strings.Cut(line, ",")
		if strings.TrimSpace(name) != "" && compactGPUStringContains(name, displayName) {
			count++
		}
	}
	return count
}

func compactGPUStringContains(output, model string) bool {
	compact := func(value string) string {
		return strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "_", "", "\n", "", "\r", "").Replace(value))
	}
	return strings.Contains(compact(output), compact(model))
}

// logRentedInstance records where and how a provider that chooses its own
// placement (Aliyun: zone, instance type, spot or on-demand) bought the VM,
// with the refusals on the way, so a fallback is visible in the job log.
func logRentedInstance(t *testing.T, backend *vmBackend, instance *cloudprovider.Instance) {
	t.Helper()
	t.Logf("%s VM %s: gpu=%s x%d region=%s zone=%s type=%s billing=%s", backend.Name, instance.InstanceID, instance.GPUModel, instance.GPUCount, instance.Region, instance.Zone, instance.InstanceType, instance.Billing)
	if attempts, ok := backend.Provider.(interface{ Attempts() []string }); ok {
		for _, line := range attempts.Attempts() {
			t.Logf("%s rental attempt: %s", backend.Name, line)
		}
	}
}

// requireInstancesRunImage asserts that every instance of a deployment was
// launched with the engine image the case chose for its card type, so a
// fallback to a different image cannot pass as the Volta run.
func requireInstancesRunImage(t *testing.T, instances []store.Instance, want string) {
	t.Helper()
	if len(instances) == 0 {
		t.Fatal("deployment has no instances to check the engine image of")
	}
	for _, instance := range instances {
		var spec proto.InstanceDesiredState
		if err := json.Unmarshal([]byte(instance.DesiredStateJSON), &spec); err != nil {
			t.Fatalf("instance %s desired state: %v", instance.UUID, err)
		}
		if !pullref.SameRef(spec.Image, want) {
			t.Fatalf("instance %s runs image %q, want %q", instance.UUID, spec.Image, want)
		}
	}
}

// gpuWarmupCommand starts the engine image on every GPU of the VM the way the
// agent does (--gpus "device=N", CUDA_VISIBLE_DEVICES=0) and creates a CUDA
// context in it. A fresh VM's first container is sometimes started before the
// GPU stack serves containers: on Aliyun 2026-10-27 the first vLLM start on a
// new A10 VM died inside torch._C._cuda_init() with "No CUDA GPUs are
// available" (and once with NVMLError_Unknown), and the platform's retry then
// started the identical container fine. Running this first, with retries,
// absorbs that instead of leaving it to a deployment's retry budget.
func gpuWarmupCommand(image string) string {
	return `set -u
n=$(nvidia-smi -L | wc -l)
[ "$n" -ge 1 ] || { echo "no GPU listed" >&2; exit 1; }
i=0
while [ "$i" -lt "$n" ]; do
  docker run --rm --gpus "\"device=$i\"" -e CUDA_VISIBLE_DEVICES=0 --entrypoint python ` + shellQuoteForTest(image) + ` -c 'import torch; assert torch.cuda.is_available(); torch.zeros(1, device="cuda"); print("GPUWORKS_GPU_READY")' 2>&1 | tail -3 | grep -q GPUWORKS_GPU_READY || exit 1
  i=$((i + 1))
done
echo "GPUWORKS_GPUS_READY $n"`
}

// waitGPUsReady runs the warm-up on every VM until it passes, within a few
// minutes, and logs how many tries the VM needed (the evidence for how often
// a cold VM's first container fails). A VM that never gets there fails the case.
func waitGPUsReady(t *testing.T, envs []*cloudTestEnv, image string) {
	t.Helper()
	for rank, env := range envs {
		var out string
		var err error
		tries := 0
		for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); {
			tries++
			out, err = runCloudCommand(t.Context(), env.Instance, gpuWarmupCommand(image))
			if err == nil && strings.Contains(out, "GPUWORKS_GPUS_READY") {
				break
			}
			select {
			case <-t.Context().Done():
				t.Fatalf("rank %d GPU warm-up canceled: %v", rank, t.Context().Err())
			case <-time.After(10 * time.Second):
			}
		}
		if err != nil || !strings.Contains(out, "GPUWORKS_GPUS_READY") {
			t.Fatalf("rank %d: containers cannot use the GPUs after %d tries: %v\n%s", rank, tries, err, strings.TrimSpace(out))
		}
		t.Logf("rank %d GPUs ready for containers after %d warm-up tr%s", rank, tries, map[bool]string{true: "y", false: "ies"}[tries == 1])
	}
}
