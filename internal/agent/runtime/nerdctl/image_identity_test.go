package nerdctl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/docker"
)

const testImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testTargetDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func TestInspectImagePreservesConfigIdentityForNameAndTargetLookup(t *testing.T) {
	target := "sha256:" + strings.Repeat("a", 64)
	config := "sha256:" + strings.Repeat("b", 64)
	// nerdctl 1.7.6 looks up target digests, not the config digest returned
	// by dockercompat image inspect. Replay both distinct native descriptors.
	raw := `{"Image":{"Name":"docker.io/example/runner:v1","Target":{"digest":"` + target + `"}},"ImageConfigDesc":{"digest":"` + config + `"},"ImageConfig":{"config":{"Entrypoint":["vllm"],"Cmd":["serve"],"Env":["LD_LIBRARY_PATH=/usr/local/lib","EMPTY="],"Volumes":{"/cache":{}}}}}`
	installFakeNerdctl(t, `
case "$*" in
  '-n gpuworks image inspect --mode=native --format {{json .}} docker.io/example/runner:v1'|'-n gpuworks image inspect --mode=native --format {{json .}} `+target+`')
    printf '%s\n' '`+raw+`' ;;
  '-n gpuworks image inspect --format {{json .}} docker.io/example/runner:v1'|'-n gpuworks image inspect --format {{json .}} `+target+`')
    printf '%s\n' '{"Id":"`+config+`","Config":{"Entrypoint":["vllm"],"Cmd":["serve"],"Env":["LD_LIBRARY_PATH=/usr/local/lib"],"Volumes":{"/cache":{}}}}' ;;
  *) echo 'unexpected command or unresolvable config digest' >&2; exit 1 ;;
esac`)
	// The same platform image must retain Docker's config identity in catalog
	// reports and after an agent upgrade, independently of its lookup reference.
	dockerDir := t.TempDir()
	dockerScript := "#!/bin/sh\nprintf '%s\\n' '{\"Id\":\"" + config + "\",\"Config\":{}}'\n"
	if err := os.WriteFile(filepath.Join(dockerDir, "docker"), []byte(dockerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dockerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	dockerImage, err := docker.New().InspectImage(context.Background(), "docker.io/example/runner:v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"docker.io/example/runner:v1", target} {
		got, err := New().InspectImage(context.Background(), ref)
		if err != nil {
			t.Fatalf("InspectImage(%q): %v", ref, err)
		}
		if got.ID != config || got.ID != dockerImage.ID || got.Env["LD_LIBRARY_PATH"] != "/usr/local/lib" || len(got.Entrypoint) != 1 || got.Entrypoint[0] != "vllm" || len(got.Cmd) != 1 || got.Cmd[0] != "serve" || len(got.Volumes) != 1 || got.Volumes[0] != "/cache" {
			t.Fatalf("InspectImage(%q) lost identity or runtime config: %+v", ref, got)
		}
	}
}

func TestWorkloadInspectionDoesNotTreatMutableNameAsIdentity(t *testing.T) {
	_, err := parseWorkloadInspection([]byte(`{"Id":"container-id","Image":"docker.io/gpuworks/gpuworks-runner:cann9.1-910b-vllm0.27.1rc1-r2","Config":{}}`))
	if err == nil {
		t.Fatal("a legacy container with only an image name cannot prove its original image identity")
	}
}

func TestRunRecordsImageIdentityWithoutChangingLaunchReference(t *testing.T) {
	dir := t.TempDir()
	labelFile := filepath.Join(dir, "label")
	argsFile := filepath.Join(dir, "args")
	retagFile := filepath.Join(dir, "retagged")
	ref := "docker.io/example/runner:v1"
	newDigest := "sha256:" + strings.Repeat("b", 64)
	installFakeNerdctl(t, `
[ "$1 $2" = '-n gpuworks' ] || exit 9
case "$3 $4" in
  'image inspect')
    for ref; do :; done
    case "$ref" in
      '`+ref+`') id='`+testImageDigest+`'; target='`+testTargetDigest+`'; if [ -e '`+retagFile+`' ]; then id='`+newDigest+`'; target='sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'; fi ;;
      '`+testTargetDigest+`') id='`+testImageDigest+`'; target='`+testTargetDigest+`' ;;
      *) echo 'no such image' >&2; exit 1 ;;
    esac
    printf '{"Image":{"Target":{"digest":"%s"}},"ImageConfigDesc":{"digest":"%s"},"ImageConfig":{"config":{}}}\n' "$target" "$id" ;;
  'run -d')
    printf '%s\n' "$@" > '`+argsFile+`'
    target=''
    for arg; do
      case "$arg" in gpuworks.image-target=*) target="${arg#gpuworks.image-target=}" ;; esac
    done
    printf '%s\n' "$target" > '`+labelFile+`'
    echo container-full-id ;;
  'container inspect')
    read -r target < '`+labelFile+`'
    printf '{"Id":"container-full-id","Image":"`+ref+`","Config":{"Labels":{"gpuworks.image-target":"%s"}}}\n' "$target" ;;
  *) exit 8 ;;
esac`)
	// Spare capacity catches accidental mutation of the caller's argument slice.
	args := make([]string, 2, 8)
	copy(args, []string{"--network", "host"})
	spec := runtime.WorkloadSpec{Name: "gpuworks-test", Image: ref, DockerArgs: args, Cmd: []string{"serve", "/model"}}
	id, err := New().Run(context.Background(), spec)
	if err != nil || id != "container-full-id" {
		t.Fatalf("run=%q err=%v", id, err)
	}
	for _, value := range args[len(args):cap(args)] {
		if value != "" {
			t.Fatal("Run mutated the caller's DockerArgs backing array")
		}
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "-n\ngpuworks\nrun\n-d\n--name\ngpuworks-test\n--network\nhost\n--label\ngpuworks.image-target=" + testTargetDigest + "\n" + ref + "\nserve\n/model\n"
	if string(data) != want {
		t.Fatalf("launch argv=%q, want original image/command plus label %q", data, want)
	}
	// The tag changes after creation. A new driver must read the stored identity,
	// not resolve the tag to the newer image or depend on an in-memory cache.
	if err := os.WriteFile(retagFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	driver := New()
	current, err := driver.InspectImage(context.Background(), ref)
	if err != nil || current.ID != newDigest {
		t.Fatalf("retag fixture did not change: %+v, %v", current, err)
	}
	workload, err := driver.InspectWorkload(context.Background(), id)
	if err != nil || workload.ImageID != "" || workload.ImageLookupRef != testTargetDigest {
		t.Fatalf("old container lost its recorded image: %+v, %v", workload, err)
	}
	image, err := driver.InspectImage(context.Background(), workload.ImageLookupRef)
	if err != nil || image.ID != testImageDigest {
		t.Fatalf("recorded image is not queryable: %+v, %v", image, err)
	}
}

func TestRunImageObservationFailureDoesNotPreventLaunch(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeNerdctl(t, `case "$3" in
image) echo 'image metadata unavailable' >&2; exit 1 ;;
run) printf '%s\n' "$@" > '`+argsFile+`'; echo running-id ;;
*) exit 9 ;;
esac`)
	id, err := New().Run(context.Background(), runtime.WorkloadSpec{Name: "x", Image: "runner:v1"})
	if err != nil || id != "running-id" {
		t.Fatalf("optional observation blocked launch: id=%q err=%v", id, err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil || strings.Contains(string(args), "--label") || !strings.HasSuffix(string(args), "runner:v1\n") {
		t.Fatalf("failed read fabricated identity or changed image: args=%q err=%v", args, err)
	}
}

func TestWorkloadImageLabelValidation(t *testing.T) {
	for _, tc := range []struct {
		name, image, target string
		wantError           bool
	}{
		{"recorded", "runner:v1", testTargetDigest, false},
		{"digest launch reference", testTargetDigest, testTargetDigest, false},
		{"conflicting target", "sha256:" + strings.Repeat("d", 64), testTargetDigest, true},
		{"legacy name", "runner:v1", "", true},
		{"legacy digest", testTargetDigest, "", true},
		{"target is name", "runner:v1", "runner:v1", true},
		{"truncated", "runner:v1", "sha256:1234", true},
		{"nonhex", "runner:v1", "sha256:" + strings.Repeat("z", 64), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"Id": "container-id", "Image": tc.image, "Config": map[string]any{"Labels": map[string]string{"gpuworks.image-target": tc.target}}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseWorkloadInspection(raw)
			if (err != nil) != tc.wantError || (!tc.wantError && (got.ImageID != "" || got.ImageLookupRef != testTargetDigest)) {
				t.Fatalf("inspection=%+v err=%v", got, err)
			}
		})
	}
}

func TestInspectImageRejectsDifferentTarget(t *testing.T) {
	raw := `{"Image":{"Target":{"digest":"` + testImageDigest + `"}},"ImageConfigDesc":{"digest":"` + testImageDigest + `"},"ImageConfig":{"config":{}}}`
	installFakeNerdctl(t, "printf '%s\\n' '"+raw+"'")
	if _, err := New().InspectImage(context.Background(), testTargetDigest); err == nil {
		t.Fatal("accepted a different target than the container's recorded digest")
	}
}

func TestInspectImageRejectsIncompleteNativeIdentity(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"Id":"` + testImageDigest + `","Config":{}}`,
		`{"Image":{"Target":{"digest":"runner:v1"}},"ImageConfig":{}}`,
		`{"Image":{"Target":{"digest":"` + testTargetDigest + `"}},"ImageConfigDesc":{"digest":"` + testImageDigest + `"}}`,
		`{"Image":{"Target":{"digest":"` + testTargetDigest + `"}},"ImageConfig":{}}`,
		`{"Image":{"Target":{"digest":"` + testTargetDigest + `"}},"ImageConfigDesc":{"digest":"runner:v1"},"ImageConfig":{}}`,
	} {
		installFakeNerdctl(t, "printf '%s\\n' '"+raw+"'")
		if _, err := New().InspectImage(context.Background(), "runner:v1"); err == nil {
			t.Fatalf("accepted incomplete native output %s", raw)
		}
	}
}
