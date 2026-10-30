package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	aliyundriver "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
)

// imageFixture answers the image calls the way ECS does: DescribeImages lists
// only Available images unless Status names the others, CopyImage needs an
// Available source, and a copy is a new image ID in the destination region.
type imageFixture struct {
	mu      sync.Mutex
	images  map[string]*fixtureImage
	next    int
	creates []map[string]string
	copies  []map[string]string
}

type fixtureImage struct {
	ID, Name, Region, Status string
	describes                int
	finishAfter              int // describes before Creating turns into the final status
	final                    string
}

func (f *imageFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	action := r.Header.Get("x-acs-action")
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	fail := func(status int, code, msg string) {
		reply(status, map[string]string{"Code": code, "Message": msg, "RequestId": "REQ"})
	}
	switch action {
	case "CreateImage":
		if q.Get("InstanceId") == "" || q.Get("ImageName") == "" {
			fail(400, "MissingParameter", "InstanceId and ImageName are required")
			return
		}
		for _, img := range f.images {
			if img.Name == q.Get("ImageName") && img.Region == q.Get("RegionId") {
				fail(400, "InvalidImageName.Duplicated", "The specified image name already exists.")
				return
			}
		}
		f.next++
		id := "m-created" + string(rune('0'+f.next))
		f.images[id] = &fixtureImage{ID: id, Name: q.Get("ImageName"), Region: q.Get("RegionId"), Status: "Creating", finishAfter: 2, final: "Available"}
		f.creates = append(f.creates, map[string]string{"instance": q.Get("InstanceId"), "region": q.Get("RegionId"), "tag": q.Get("Tag.1.Key")})
		reply(200, map[string]string{"RequestId": "REQ", "ImageId": id})
	case "CopyImage":
		src := f.images[q.Get("ImageId")]
		if src == nil || src.Region != q.Get("RegionId") {
			fail(404, "InvalidImageId.NotFound", "The specified ImageId does not exist.")
			return
		}
		if src.Status != "Available" {
			fail(400, "IncorrectImageStatus", "The specified image is not Available.")
			return
		}
		if q.Get("DestinationRegionId") == "" || q.Get("DestinationRegionId") == q.Get("RegionId") {
			fail(400, "InvalidDestinationRegionId.Malformed", "DestinationRegionId must be another region.")
			return
		}
		f.next++
		id := "m-copy" + string(rune('0'+f.next))
		f.images[id] = &fixtureImage{ID: id, Name: q.Get("DestinationImageName"), Region: q.Get("DestinationRegionId"), Status: "Creating", finishAfter: 1, final: "Available"}
		f.copies = append(f.copies, map[string]string{"to": q.Get("DestinationRegionId")})
		reply(200, map[string]string{"RequestId": "REQ", "ImageId": id})
	case "DescribeImages":
		wantStatus := map[string]bool{"Available": true}
		if s := q.Get("Status"); s != "" {
			wantStatus = map[string]bool{}
			for _, part := range strings.Split(s, ",") {
				wantStatus[part] = true
			}
		}
		var out []map[string]string
		for _, img := range f.images {
			if img.Region != q.Get("RegionId") || (q.Get("ImageId") != "" && img.ID != q.Get("ImageId")) || (q.Get("ImageName") != "" && img.Name != q.Get("ImageName")) {
				continue
			}
			if img.Status == "Creating" {
				img.describes++
				if img.describes > img.finishAfter {
					img.Status = img.final
				}
			}
			if wantStatus[img.Status] {
				out = append(out, map[string]string{"ImageId": img.ID, "ImageName": img.Name, "Status": img.Status, "Progress": "50%"})
			}
		}
		reply(200, map[string]any{"RequestId": "REQ", "Images": map[string]any{"Image": out}})
	default:
		fail(400, "InvalidAction.NotFound", action)
	}
}

func newImageAPI(t *testing.T) (imageAPI, *imageFixture) {
	t.Helper()
	f := &imageFixture{images: map[string]*fixtureImage{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return imageAPI{rpc: aliyundriver.NewRPC(), creds: serverprovider.Credentials{PublicKey: "id", PrivateKey: "secret", Endpoint: srv.URL}}, f
}

func TestImageLifecycleCreateWaitAndCopy(t *testing.T) {
	api, f := newImageAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := api.create(ctx, "cn-beijing", "i-source", "golden-1", "desc")
	if err != nil {
		t.Fatal(err)
	}
	// A copy of an image that is still being made is refused by the service.
	if _, err := api.copy(ctx, "cn-beijing", id, "cn-hangzhou", "golden-1", "d"); err == nil || aliyundriver.ErrorCode(err) != "IncorrectImageStatus" {
		t.Fatalf("copy of a Creating image = %v, want IncorrectImageStatus", err)
	}
	var progress []string
	if err := api.wait(ctx, "cn-beijing", id, time.Millisecond, func(line string) { progress = append(progress, line) }); err != nil {
		t.Fatal(err)
	}
	if len(progress) == 0 || !strings.Contains(progress[0], "status=Creating") {
		t.Fatalf("wait reported no progress while the image was Creating: %v", progress)
	}
	copyID, err := api.copy(ctx, "cn-beijing", id, "cn-hangzhou", "golden-1", "d")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.wait(ctx, "cn-hangzhou", copyID, time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 || f.creates[0]["instance"] != "i-source" || f.creates[0]["tag"] != "gpuworks-golden" {
		t.Fatalf("CreateImage requests = %v", f.creates)
	}
	if _, err := api.create(ctx, "cn-beijing", "i-source", "golden-1", "d"); aliyundriver.ErrorCode(err) != "InvalidImageName.Duplicated" {
		t.Fatalf("a duplicate image name must surface the API error, got %v", err)
	}
	found, err := api.byName(ctx, "cn-hangzhou", "golden-1")
	if err != nil || found.ImageID != copyID {
		t.Fatalf("byName = %+v, %v", found, err)
	}
	if _, err := api.byName(ctx, "cn-hangzhou", "nope"); err == nil {
		t.Fatal("an unknown image name must be an error")
	}
}

func TestWaitStopsAtOnceWhenTheImageFails(t *testing.T) {
	api, f := newImageAPI(t)
	f.images["m-bad"] = &fixtureImage{ID: "m-bad", Name: "bad", Region: "cn-beijing", Status: "Creating", finishAfter: 1, final: "CreateFailed"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := api.wait(ctx, "cn-beijing", "m-bad", time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "CreateFailed") {
		t.Fatalf("wait on a failed image = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("wait ran out the clock instead of failing at once")
	}
}

func TestCopyEverywhereCopiesToEveryOtherRegionAndReportsEachID(t *testing.T) {
	api, f := newImageAPI(t)
	f.images["m-src"] = &fixtureImage{ID: "m-src", Name: "golden", Region: "cn-beijing", Status: "Available"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	o := options{regions: []string{"cn-beijing", "cn-hangzhou", "cn-shenzhen"}, copyPoll: time.Millisecond}
	ids, err := copyEverywhere(ctx, api, o, "cn-beijing", "m-src", "golden")
	if err != nil {
		t.Fatal(err)
	}
	if ids["cn-beijing"] != "m-src" || ids["cn-hangzhou"] == "" || ids["cn-shenzhen"] == "" || len(ids) != 3 {
		t.Fatalf("ids = %v", ids)
	}
	if len(f.copies) != 2 {
		t.Fatalf("copies = %v, want one per other region and none to the source region", f.copies)
	}
	// A source the service refuses to copy is reported with the API's reason.
	f.images["m-src"].Status = "UnAvailable"
	if _, err := copyEverywhere(ctx, api, options{regions: []string{"cn-beijing", "cn-guangzhou"}, copyPoll: time.Millisecond}, "cn-beijing", "m-src", "golden"); err == nil || !strings.Contains(err.Error(), "IncorrectImageStatus") {
		t.Fatalf("copy from an unusable source = %v", err)
	}
}

func TestEnvVarNameMatchesWhatTheHarnessReads(t *testing.T) {
	if got := envVarName("cn-hangzhou"); got != "ALIYUN_IMAGE_CN_HANGZHOU" {
		t.Fatalf("envVarName = %q", got)
	}
}

func TestInstallDockerCommandPinsDockerAndUsesMainlandMirrors(t *testing.T) {
	command := installDockerCommand()
	for _, want := range []string{
		"mirrors.aliyun.com/docker-ce/linux/ubuntu", `5:29\.`, "nvidia-ctk runtime configure --runtime=docker",
		"GPUWORKS_DOCKER_INSTALL_EVIDENCE", "docker info --format '{{json .Runtimes}}' | grep -q nvidia",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("install command lacks %q", want)
		}
	}
	if strings.Contains(command, "download.docker.com") {
		t.Error("install command reaches Docker's own site, which mainland instances cannot rely on")
	}
	if strings.Contains(command, "sudo") {
		t.Error("the builder runs as root; sudo would only add a failure mode")
	}
	if strings.Index(command, "rm -f /etc/apt/sources.list.d/docker.list") > strings.Index(command, "apt-get update") {
		t.Error("a resumed build must drop the Docker repository of the earlier attempt before the base apt update")
	}
	if aliyun, tuna := strings.Index(command, "mirrors.aliyun.com/docker-ce"), strings.Index(command, "mirrors.tuna.tsinghua.edu.cn/docker-ce"); aliyun < 0 || tuna < aliyun {
		t.Error("a Docker CE mirror caught mid-sync must fall back to the next mirror, the cloud's own first")
	}
	if strings.Index(command, "mirrors.aliyun.com/nvidia-container-toolkit") > strings.Index(command, "nvidia.github.io") {
		t.Error("the toolkit mirrors must be tried before nvidia.github.io")
	}
}

func TestPullImageCommandTriesMirrorsInOrderAndTagsTheCanonicalName(t *testing.T) {
	command := pullImageCommand("gpustack/runner:cuda12.8-vllm0.17.1", []string{"swr.example/gpustack/runner:x", "gpustack/runner:cuda12.8-vllm0.17.1"})
	if strings.Index(command, "swr.example") > strings.Index(command, "docker pull 'gpustack/runner") {
		t.Fatalf("mirror must come first:\n%s", command)
	}
	if !strings.Contains(command, `docker tag "$ok" 'gpustack/runner:cuda12.8-vllm0.17.1'`) {
		t.Fatalf("the pulled image is not tagged with the name the cases deploy:\n%s", command)
	}
}

func TestBootedChecksRequireModelImageRuntimeAndAGPUInAContainer(t *testing.T) {
	command := bootedChecksCommand("/opt/gpuworks/test-fixtures/models/qwen2-0.5b", "gpustack/runner:cuda12.8-vllm0.17.1")
	for _, want := range []string{"sha256sum -c SHA256SUMS", "docker image inspect", "grep -q nvidia", "docker run --rm --gpus all --entrypoint nvidia-smi", "GPUWORKS_GOLDEN_OK"} {
		if !strings.Contains(command, want) {
			t.Errorf("booted-image checks lack %q", want)
		}
	}
	if !strings.HasPrefix(command, "set -eu") {
		t.Error("checks must abort on the first failure")
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" cn-hangzhou, cn-beijing\ncn-shenzhen ,,")
	if strings.Join(got, "|") != "cn-hangzhou|cn-beijing|cn-shenzhen" {
		t.Fatalf("splitList = %v", got)
	}
}

func TestImageNameAndDescriptionFollowTheModel(t *testing.T) {
	for repo, want := range map[string]string{"Qwen/Qwen3-0.6B": "qwen3-0-6b", "Qwen/Qwen2-0.5B-Instruct": "qwen2-0-5b", "plain_name": "plain-name"} {
		if got := modelSlug(repo); got != want {
			t.Fatalf("modelSlug(%q) = %q, want %q", repo, got, want)
		}
	}
	o := options{modelRepo: "Qwen/Qwen3-0.6B", modelPath: "/opt/m", dockerImage: "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2"}
	if got := imageDescription(o); !strings.Contains(got, o.modelRepo) || !strings.Contains(got, o.dockerImage) || len(got) > 256 {
		t.Fatalf("description = %q", got)
	}
	o.dockerImage = strings.Repeat("x", 300)
	if got := imageDescription(o); len(got) != 256 {
		t.Fatalf("description is %d chars, want capped at 256", len(got))
	}
}
