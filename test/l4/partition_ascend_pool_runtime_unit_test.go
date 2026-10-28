//go:build cloudgpu && partition

package l4

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const ascendTestImage = "172.20.9.4:10043/ai-model-registry/vllm-ascend:0.26.0rc1"

func TestValidateAscendRunveilCache(t *testing.T) {
	for _, tc := range []struct {
		cache string
		ok    bool
	}{
		{defaultAscendRunveilCache, true},
		{"/opt/gpuworks-l4-other", true},
		{"/tmp/gpuworks-fixture", true},
		{"/opt/gpuworks-l4-", false},
		{"/opt/gpuworks-mr642", false}, // another tenant's agent dir
		{"/opt/gpuworks", false},
		{"/opt/gpuworks-l4-x/../gpuworks-mr642", false},
		{"/opt/gpuworks-l4-x/", false},
		{"opt/gpuworks-l4-x", false},
		{"", false},
	} {
		if err := validateAscendRunveilCache(tc.cache); (err == nil) != tc.ok {
			t.Errorf("validateAscendRunveilCache(%q) = %v, want ok=%v", tc.cache, err, tc.ok)
		}
	}
}

func TestAscendPoolRemoteRootKeepsRunveilRunsOffTmpfs(t *testing.T) {
	for _, runtime := range []string{"docker", "nerdctl"} {
		if got := ascendPoolRemoteRoot(runtime, defaultAscendRunveilCache); got != "/tmp" {
			t.Errorf("%s run root = %q, want /tmp", runtime, got)
		}
	}
	// /tmp is tmpfs on the pool hosts: an 18 GiB rootfs per container belongs
	// on the disk that holds the cache, so the staged hard links stay valid.
	if got := ascendPoolRemoteRoot("udocker", defaultAscendRunveilCache); got != defaultAscendRunveilCache+"-run" {
		t.Errorf("udocker run root = %q", got)
	}
	if got := ascendPoolRemoteRoot("udocker", "/tmp/gpuworks-fixture"); got != "/tmp/gpuworks-fixture-run" {
		t.Errorf("udocker run root for a /tmp cache = %q", got)
	}
}

func TestParseAscendPoolPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, out, card, runtime, model string
	}{
		{"udocker member", "Authorized users only.\ncard=3\nruntime=udocker\nmodel=/opt/x/models/m\n", "3", "udocker", "/opt/x/models/m"},
		{"nerdctl member", "card=5\nruntime=nerdctl\nmodel=/opt/gpuworks/models/m\n", "5", "nerdctl", "/opt/gpuworks/models/m"},
		{"banner only", "Authorized users only. All activities may be monitored.\n", "", "", ""},
		{"empty", "", "", "", ""},
	} {
		card, runtime, model := parseAscendPoolPreflight(tc.out)
		if card != tc.card || runtime != tc.runtime || model != tc.model {
			t.Errorf("%s: got card=%q runtime=%q model=%q, want card=%q runtime=%q model=%q", tc.name, card, runtime, model, tc.card, tc.runtime, tc.model)
		}
	}
}

// A member host as the preflight sees it: which container CLIs exist and work,
// and what the runveil cache holds. Each field maps to one stub binary or
// directory in a PATH that contains nothing else, so the machine running the
// test can have its own docker without changing the outcome.
type ascendMemberFixture struct {
	docker         string // "", "ok" (daemon up, has image), "no-image", "daemon-down"
	nerdctl        string // "", "ok" (has image in the gpuworks namespace), "wrong-namespace", "daemon-down"
	cacheTag       bool   // repos/<repo>/<tag>/TAG
	cacheManifest  bool
	cacheLayersDir bool
	sharedModel    bool // the shared model directory exists (root-only on the real hosts)
	runveilModel   bool // the world-readable copy next to the cache exists
}

func (f ascendMemberFixture) preflight(t *testing.T) (out string, err error) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	model := filepath.Join(root, "model")
	cache := filepath.Join(root, "cache")
	runveilModel := filepath.Join(cache, "models", "model")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if f.sharedModel {
		write(filepath.Join(model, "config.json"), "{}")
	}
	if f.runveilModel {
		write(filepath.Join(runveilModel, "config.json"), "{}")
	}
	shell, lookErr := exec.LookPath("bash")
	if lookErr != nil {
		t.Skip("bash is required to run the preflight script")
	}
	for _, tool := range []string{"awk", "grep", "head"} {
		target, lookErr := exec.LookPath(tool)
		if lookErr != nil {
			t.Skipf("%s is required to run the preflight script", tool)
		}
		if err := os.Symlink(target, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "uname"), "#!/bin/sh\necho aarch64\n")
	write(filepath.Join(bin, "gzip"), "#!/bin/sh\nexit 0\n")
	write(filepath.Join(bin, "curl"), "#!/bin/sh\nexit 0\n")
	write(filepath.Join(root, "npu-smi.out"), ascendNPUSMIShared)
	write(filepath.Join(bin, "npu-smi"), "#!/bin/sh\nwhile IFS= read -r line; do printf '%s\\n' \"$line\"; done < '"+filepath.Join(root, "npu-smi.out")+"'\n")
	switch f.docker {
	case "ok":
		write(filepath.Join(bin, "docker"), "#!/bin/sh\ncase \"$1\" in info) exit 0;; image) test \"$2\" = inspect && test \"$3\" = '"+ascendTestImage+"';; *) exit 1;; esac\n")
	case "no-image":
		write(filepath.Join(bin, "docker"), "#!/bin/sh\ncase \"$1\" in info) exit 0;; *) echo 'Error: No such image' >&2; exit 1;; esac\n")
	case "daemon-down":
		write(filepath.Join(bin, "docker"), "#!/bin/sh\necho 'Cannot connect to the Docker daemon' >&2; exit 1\n")
	}
	switch f.nerdctl {
	case "ok":
		// The agent pins -n gpuworks; an unpinned lookup misses the image.
		write(filepath.Join(bin, "nerdctl"), "#!/bin/sh\nif test \"$1\" = info; then exit 0; fi\ntest \"$1\" = -n && test \"$2\" = gpuworks && test \"$3\" = image && test \"$4\" = inspect && test \"$5\" = '"+ascendTestImage+"'\n")
	case "wrong-namespace":
		write(filepath.Join(bin, "nerdctl"), "#!/bin/sh\nif test \"$1\" = info; then exit 0; fi\nexit 1\n")
	case "daemon-down":
		write(filepath.Join(bin, "nerdctl"), "#!/bin/sh\necho 'cannot connect to containerd' >&2; exit 1\n")
	}
	tagDir := filepath.Join(cache, "repos", "172.20.9.4:10043", "ai-model-registry", "vllm-ascend", "0.26.0rc1")
	if f.cacheTag {
		write(filepath.Join(tagDir, "TAG"), "172.20.9.4:10043/ai-model-registry/vllm-ascend:0.26.0rc1")
	}
	if f.cacheManifest {
		write(filepath.Join(tagDir, "manifest"), "{}")
	}
	if f.cacheLayersDir {
		if err := os.MkdirAll(filepath.Join(cache, "layers"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(shell, "-c", ascendPoolPreflightScript("3", model, runveilModel, ascendTestImage, cache))
	cmd.Env = []string{"PATH=" + bin}
	combined, runErr := cmd.CombinedOutput()
	return string(combined), runErr
}

// Runtime is decided per member from what that member has, instead of one
// global switch that forces every member onto the same runtime.
func TestAscendPoolPreflightPicksRuntimePerMember(t *testing.T) {
	fullCache := ascendMemberFixture{cacheTag: true, cacheManifest: true, cacheLayersDir: true, sharedModel: true, runveilModel: true}
	withCache := func(f ascendMemberFixture) ascendMemberFixture {
		f.cacheTag, f.cacheManifest, f.cacheLayersDir, f.sharedModel, f.runveilModel = true, true, true, true, true
		return f
	}
	for _, tc := range []struct {
		name    string
		member  ascendMemberFixture
		runtime string // empty: the member must be rejected
		model   string // "shared" or "runveil": which fixture the member serves
		reject  string
	}{
		{name: "docker with the image", member: ascendMemberFixture{docker: "ok", sharedModel: true}, runtime: "docker", model: "shared"},
		{name: "nerdctl with the image in the gpuworks namespace", member: ascendMemberFixture{nerdctl: "ok", sharedModel: true}, runtime: "nerdctl", model: "shared"},
		{name: "docker wins over a cache it also has", member: withCache(ascendMemberFixture{docker: "ok"}), runtime: "docker", model: "shared"},
		{name: "nerdctl wins over a cache it also has", member: withCache(ascendMemberFixture{nerdctl: "ok"}), runtime: "nerdctl", model: "shared"},
		{name: "neither docker nor nerdctl but a prepared cache", member: fullCache, runtime: "udocker", model: "runveil"},
		{name: "dead docker daemon falls through to the cache", member: withCache(ascendMemberFixture{docker: "daemon-down"}), runtime: "udocker", model: "runveil"},
		{name: "dead containerd falls through to the cache", member: withCache(ascendMemberFixture{nerdctl: "daemon-down"}), runtime: "udocker", model: "runveil"},
		{name: "docker without the image falls through to the cache", member: withCache(ascendMemberFixture{docker: "no-image"}), runtime: "udocker", model: "runveil"},
		{name: "nerdctl without the image in its namespace falls through to the cache", member: withCache(ascendMemberFixture{nerdctl: "wrong-namespace"}), runtime: "udocker", model: "runveil"},
		{name: "nothing at all", member: ascendMemberFixture{sharedModel: true}, reject: "not present in docker"},
		{name: "runveil member without the readable model copy", member: ascendMemberFixture{cacheTag: true, cacheManifest: true, cacheLayersDir: true, sharedModel: true}, reject: "model fixture missing"},
		{name: "docker member without the shared model", member: ascendMemberFixture{docker: "ok", runveilModel: true}, reject: "model fixture missing"},
		{name: "docker without the image and no cache", member: ascendMemberFixture{docker: "no-image"}, reject: "runveil cache"},
		{name: "nerdctl in the wrong namespace and no cache", member: ascendMemberFixture{nerdctl: "wrong-namespace"}, reject: "runveil cache"},
		{name: "cache without a registered tag", member: ascendMemberFixture{cacheManifest: true, cacheLayersDir: true}, reject: "runveil cache"},
		{name: "cache with a tag but no manifest", member: ascendMemberFixture{cacheTag: true, cacheLayersDir: true}, reject: "runveil cache"},
		{name: "cache with a tag but no layers", member: ascendMemberFixture{cacheTag: true, cacheManifest: true}, reject: "runveil cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.member.preflight(t)
			card, runtime, model := parseAscendPoolPreflight(out)
			if tc.runtime == "" {
				if err == nil {
					t.Fatalf("member was accepted (runtime=%q), want rejection:\n%s", runtime, out)
				}
				if !strings.Contains(out, tc.reject) {
					t.Fatalf("rejection does not say why (%q missing):\n%s", tc.reject, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("member rejected: %v\n%s", err, out)
			}
			if runtime != tc.runtime {
				t.Fatalf("runtime = %q, want %q\n%s", runtime, tc.runtime, out)
			}
			if (tc.model == "runveil") != strings.Contains(model, "/cache/models/") {
				t.Fatalf("model = %q for a %s member, want the %s fixture", model, runtime, tc.model)
			}
			if card != "2" {
				// ascendNPUSMIShared: card 0 runs sglang, card 1 holds hidden HBM.
				t.Fatalf("card = %q, want the first idle card 2", card)
			}
		})
	}
}

func TestAscendPoolPreflightNamesTheCacheItLookedIn(t *testing.T) {
	script := ascendPoolPreflightScript("3", "/models/m", "/opt/gpuworks-l4-runveil-cache/models/m", ascendTestImage, "/opt/gpuworks-l4-runveil-cache")
	for _, want := range []string{
		"/opt/gpuworks-l4-runveil-cache/repos/172.20.9.4:10043/ai-model-registry/vllm-ascend/0.26.0rc1/TAG",
		"/opt/gpuworks-l4-runveil-cache/layers",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("preflight script does not check %s:\n%s", want, script)
		}
	}
}
