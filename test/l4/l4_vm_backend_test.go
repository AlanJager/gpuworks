//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

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
