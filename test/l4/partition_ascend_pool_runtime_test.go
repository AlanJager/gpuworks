//go:build cloudgpu && partition

package l4

import (
	"fmt"
	"path"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// defaultAscendRunveilCache is where a pool member without docker/nerdctl
// keeps the acceptance image in udocker's store format (repos/ + layers/). It
// is a dedicated directory, not any agent's own udocker dir, and it must live
// on the same filesystem as its sibling run root (see ascendPoolRemoteRoot)
// because each temporary agent receives hard links, not copies.
const defaultAscendRunveilCache = "/opt/gpuworks-l4-runveil-cache"

// ascendRunveilCachePath is the per-host runveil image cache. Every member
// uses the same path; a member without the directory simply is not a runveil
// member.
func ascendRunveilCachePath() string {
	return hostEnv("GPUWORKS_ASCEND_L4_RUNVEIL_CACHE", defaultAscendRunveilCache)
}

// validateAscendRunveilCache keeps the cache (a hard-link source on a shared
// host) inside directories reserved for this test.
func validateAscendRunveilCache(cache string) error {
	if !path.IsAbs(cache) || path.Clean(cache) != cache || strings.Contains(cache, "..") {
		return fmt.Errorf("runveil cache %q must be a clean absolute path", cache)
	}
	for _, prefix := range []string{"/opt/gpuworks-l4-", "/tmp/gpuworks-"} {
		if strings.HasPrefix(cache, prefix) && len(cache) > len(prefix) {
			return nil
		}
	}
	return fmt.Errorf("runveil cache %q must be an isolated /opt/gpuworks-l4-* or /tmp/gpuworks-* directory", cache)
}

// ascendPoolRemoteRoot is the directory that holds a temporary agent's run
// directory. nerdctl and docker members keep the historical /tmp. A runveil
// member extracts every container rootfs (18 GiB for the acceptance image)
// into its state directory, and /tmp is RAM-backed on the pool hosts, so it
// gets a disk-backed sibling of the cache instead: same filesystem as the
// cache, which is what makes the hard-linked image stage work. A cache that
// itself lives under /tmp keeps the run root next to it.
func ascendPoolRemoteRoot(runtime, cache string) string {
	if runtime != "udocker" {
		return "/tmp"
	}
	return cache + "-run"
}

// ascendPoolRuntimeProbe is the shell fragment that decides which runtime a
// member offers for image, sets $runtime and prints runtime=<name>. docker and nerdctl win
// when they work and already hold the image; otherwise a member whose runveil
// cache holds the image is a udocker member. A host with neither fails with
// the three places it looked.
func ascendPoolRuntimeProbe(image, cache string) string {
	ref := native.ParseRef(image)
	tagDir := path.Join(cache, "repos", ref.Local, ref.Tag)
	return fmt.Sprintf(`if command -v docker >/dev/null && docker info >/dev/null 2>&1 && docker image inspect %[1]s >/dev/null 2>&1; then
  runtime=docker
elif command -v nerdctl >/dev/null && nerdctl info >/dev/null 2>&1 && nerdctl -n gpuworks image inspect %[1]s >/dev/null 2>&1; then
  runtime=nerdctl
elif test -f %[2]s && test -f %[3]s && test -d %[4]s; then
  runtime=udocker
else
  fail "acceptance image %[5]s not present in docker, in nerdctl's gpuworks namespace (the agent runs containers there), or in the runveil cache %[6]s"
fi
echo runtime="$runtime"`, shellQuoteL4(image), shellQuoteL4(path.Join(tagDir, "TAG")), shellQuoteL4(path.Join(tagDir, "manifest")), shellQuoteL4(path.Join(cache, "layers")), image, cache)
}

// ascendRunveilModelPath is the model fixture a runveil member serves. The
// shared host model directories are root-only (0700 under a 0711 parent), and
// a runveil workload runs as the unprivileged udocker user, which makes
// vLLM fail with "Invalid repository ID or local directory" on /model. The
// runveil fixture is therefore a world-readable copy kept next to the image
// cache; other runtimes keep the model path they always used.
func ascendRunveilModelPath(model, cache string) string {
	return hostEnv("GPUWORKS_ASCEND_L4_RUNVEIL_MODEL", path.Join(cache, "models", path.Base(model)))
}

// ascendPoolPreflightScript is the whole per-member check; it ends with the
// runtime and the model fixture that runtime serves. Every check
// reports which condition failed on stderr: a bare `set -eu` script exits
// silently and the CI log only shows the login banner plus "exit status 1",
// which is what hid the September fixture/Image degradation for days.
func ascendPoolPreflightScript(preferredCard, model, runveilModel, image, cache string) string {
	return fmt.Sprintf(`fail() { echo "ascend-pool preflight: $1" >&2; exit 1; }
test "$(uname -m)" = aarch64 || fail "arch is $(uname -m), want aarch64"
command -v npu-smi >/dev/null || fail "npu-smi missing"
command -v gzip >/dev/null || fail "gzip missing"
command -v curl >/dev/null || fail "curl missing"
info=$(npu-smi info) || fail "npu-smi info failed"
%s
test -n "$selected" || fail "no idle NPU: preferred %s and every other card either runs a process or holds more than %d MB of HBM (container/vNPU tenants hold HBM without listing a process)"
echo card="$selected"
%s
case "$runtime" in udocker) model=%s ;; *) model=%s ;; esac
test -f "$model/config.json" || fail "model fixture missing at $model (runtime $runtime)"
echo model="$model"`, ascendIdleCardSelection(preferredCard), preferredCard, ascendPoolMaxIdleHBMMB(), ascendPoolRuntimeProbe(image, cache), shellQuoteL4(runveilModel), shellQuoteL4(model))
}

// parseAscendPoolPreflight reads the card, runtime and model lines of a
// successful preflight; login banners and other noise are ignored.
func parseAscendPoolPreflight(out string) (card, runtime, model string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "runtime="); ok {
			runtime = value
		}
		if value, ok := strings.CutPrefix(line, "card="); ok {
			card = value
		}
		if value, ok := strings.CutPrefix(line, "model="); ok {
			model = value
		}
	}
	return card, runtime, model
}
