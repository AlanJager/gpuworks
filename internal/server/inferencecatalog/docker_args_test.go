package inferencecatalog

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const dockerArgsSpecYAML = `models:
- model_id: Org/Model
  specs:
  - name: deepinfer
    engine: vllm
    image: vllm/vllm-openai:v0.25.1
    weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}
    gpus: {vendor: nvidia}
    docker_args: [--cap-add=SYS_PTRACE, --security-opt=seccomp=unconfined]
`

// docker_args is read from the catalog document, survives the store
// projection and comes back out of it, so the editor's YAML round-trips it.
func TestSpecDockerArgsParsesAndRoundTripsThroughTheStoreRows(t *testing.T) {
	catalog, err := ParseYAML([]byte(dockerArgsSpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--cap-add=SYS_PTRACE", "--security-opt=seccomp=unconfined"}
	if got := catalog.Models[0].Specs[0].DockerArgs; !reflect.DeepEqual(got, want) {
		t.Fatalf("docker_args = %v, want %v", got, want)
	}
	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0].Specs[0].DockerArgs; got != `["--cap-add=SYS_PTRACE","--security-opt=seccomp=unconfined"]` {
		t.Fatalf("stored docker_args = %q", got)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil || !reflect.DeepEqual(restored.Specs[0].DockerArgs, want) {
		t.Fatalf("restored = %+v, %v", restored.Specs, err)
	}

	// A spec that states none decodes back to no docker_args (the stored
	// column may be "" or the JSON "null" of an absent slice; Args, the
	// column this one shares its shape with, is the same way).
	rows, err = ToFlatStoreModels(flatFixture())
	plain, err2 := FromFlatStoreModel(rows[0])
	if err != nil || err2 != nil || len(plain.Specs[0].DockerArgs) != 0 {
		t.Fatalf("plain restored = %+v, %v, %v", plain.Specs, err, err2)
	}
}

// A spec's docker_args is judged exactly like a deployment's own
// container_overrides.docker_args (deploy.ValidateDockerArgs): platform-managed
// options are refused, and so is --ipc=host together with the same spec's own
// shm_bytes. Host-access options (--privileged, --pid) are allowed.
func TestSpecDockerArgsRejectsManagedOptions(t *testing.T) {
	for name, test := range map[string]struct {
		dockerArgs string
		shmBytes   string
		code       string
	}{
		"--network=host":            {dockerArgs: "[--network=host]", code: deploy.ErrorDockerArgPlatformManaged},
		"--shm-size":                {dockerArgs: "[--shm-size=8g]", code: deploy.ErrorDockerArgUseShmField},
		"-v /x:/y":                  {dockerArgs: "[-v, '/x:/y']", code: deploy.ErrorDockerArgUseMountField},
		"--ipc=host with shm_bytes": {dockerArgs: "[--ipc=host]", shmBytes: "shm_bytes: 8589934592", code: deploy.ErrorShmSizeWithHostIPC},
	} {
		t.Run(name, func(t *testing.T) {
			yaml := strings.Replace(dockerArgsSpecYAML, "docker_args: [--cap-add=SYS_PTRACE, --security-opt=seccomp=unconfined]",
				"docker_args: "+test.dockerArgs+"\n    "+test.shmBytes, 1)
			_, err := ParseYAML([]byte(yaml))
			if err == nil {
				t.Fatalf("%s accepted", name)
			}
			if test.code == "" {
				return
			}
			var productErr *messages.ProductError
			if !errors.As(err, &productErr) || productErr.Code() != test.code {
				t.Fatalf("%s: err=%v, want code %s", name, err, test.code)
			}
		})
	}
	// --ipc=host without a stated shm_bytes is not a conflict: the spec just
	// shares the host's /dev/shm.
	plain := strings.Replace(dockerArgsSpecYAML, "docker_args: [--cap-add=SYS_PTRACE, --security-opt=seccomp=unconfined]",
		"docker_args: [--ipc=host]", 1)
	if _, err := ParseYAML([]byte(plain)); err != nil {
		t.Fatalf("--ipc=host without shm_bytes rejected: %v", err)
	}
	hostAccess := strings.Replace(dockerArgsSpecYAML, "docker_args: [--cap-add=SYS_PTRACE, --security-opt=seccomp=unconfined]",
		"docker_args: [--privileged, --pid=host]", 1)
	if _, err := ParseYAML([]byte(hostAccess)); err != nil {
		t.Fatalf("--privileged and --pid=host rejected: %v", err)
	}
}

func TestSpecDockerArgsRejectsEmptyOrWhitespaceEntries(t *testing.T) {
	for _, value := range []string{"['']", "['   ']", "[--cap-add=SYS_NICE, '']"} {
		yaml := strings.Replace(dockerArgsSpecYAML, "[--cap-add=SYS_PTRACE, --security-opt=seccomp=unconfined]", value, 1)
		if _, err := ParseYAML([]byte(yaml)); err == nil {
			t.Fatalf("docker_args %s accepted", value)
		}
	}
}

func TestSpecDockerArgsIsInTheEditorSchema(t *testing.T) {
	schema := JSONSchema()
	modelProps := schemaProperties(items(schemaProperties(schema)["models"].(map[string]any)))
	specProps := schemaProperties(items(modelProps["specs"].(map[string]any)))
	property, ok := specProps["docker_args"].(map[string]any)
	if !ok {
		t.Fatal("editor schema does not describe docker_args")
	}
	if !reflect.DeepEqual(property["type"], "array") {
		t.Errorf("schema docker_args type = %#v", property["type"])
	}
	if description, _ := property["description"].(string); !strings.Contains(description, "docker_args") || !strings.Contains(description, "ipc=host") {
		t.Errorf("schema docker_args description = %q", description)
	}
}
