package seed

import (
	"slices"
	"strings"
	"testing"
)

func TestMossTTSRealtimeMountsItsAudioTokenizer(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "openmoss-team/moss-tts-realtime" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-vllm-nvidia-general" {
				continue
			}
			if spec.Image != "gpuworks/gpuworks-runner:cuda13.0-vllm-omni0.30.0-r4" {
				t.Fatalf("image=%q, want VoxCPM/MOSS-capable r4 runner", spec.Image)
			}
			if len(spec.Components) != 2 ||
				spec.Components[1].MountPath != "/models/moss-audio-tokenizer" {
				t.Fatalf("components = %+v, want primary and audio tokenizer", spec.Components)
			}
			if len(spec.Entrypoint) != 2 || spec.Entrypoint[0] != "vllm" || spec.Entrypoint[1] != "serve" {
				t.Fatalf("entrypoint=%v, want vllm serve; the adapter adds the primary mount once", spec.Entrypoint)
			}
			if slices.Contains(spec.Entrypoint, "/models/MOSS-TTS-Realtime") {
				t.Fatalf("entrypoint=%v duplicates the adapter-injected model path", spec.Entrypoint)
			}
			args := strings.Join(spec.Args, " ")
			for _, want := range []string{
				"--omni",
				"--trust-remote-code",
				`codec_model_name_or_path":"/models/moss-audio-tokenizer`,
			} {
				if !strings.Contains(args, want) {
					t.Errorf("launch arguments do not contain %q: %v", want, spec.Args)
				}
			}
			// Both codec copies take about 15 GiB of the card, so below 40 GiB the
			// talker stage must run at a smaller share than the bundled 0.60 (an
			// RTX 4090 D OOMs at 0.35 and serves at 0.25).
			if !strings.Contains(args, "--deploy-config /tmp/moss-tts-realtime-deploy.yaml") ||
				!strings.Contains(spec.PreStartScript, "gpu_memory_utilization: ${TALKER_GPU_MEMORY_UTILIZATION}") {
				t.Errorf("the talker share is not wired through a deploy overlay: args=%v pre_start=%q", spec.Args, spec.PreStartScript)
			}
			resolver := ""
			if spec.Placement != nil {
				resolver = spec.Placement.Resolver
			}
			for _, want := range []string{"gpu.min_memory_gib < 22", "gpu.min_memory_gib < 40", `"TALKER_GPU_MEMORY_UTILIZATION": "0.25"`, `"TALKER_GPU_MEMORY_UTILIZATION": "0.60"`} {
				if !strings.Contains(resolver, want) {
					t.Errorf("placement resolver lacks %q:\n%s", want, resolver)
				}
			}
			return
		}
		t.Fatal("MOSS-TTS-Realtime NVIDIA spec is missing")
	}
	t.Fatal("MOSS-TTS-Realtime model is missing")
}
