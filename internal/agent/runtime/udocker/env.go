package udocker

import (
	"path"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/runveil"
)

const libraryPathEnv = "LD_LIBRARY_PATH"

// runEnv assembles the container environment: the loader/NCCL/nvidia-shim vars
// that make CUDA work under proot (from the runpod evaluation), GPU selection,
// and the caller's spec env.
func (d *Driver) runEnv(spec runtime.WorkloadSpec) map[string]string {
	env := map[string]string{}
	if !isDomesticSpec(spec) {
		env = map[string]string{
			"TRITON_LIBCUDA_PATH":                  "/opt/nvidia-shim",
			libraryPathEnv:                         "/opt/nvidia-shim:/usr/local/nvidia/lib64:/usr/local/nvidia/lib:/usr/local/cuda/lib64",
			"NCCL_SOCKET_IFNAME":                   "lo",
			"NCCL_IB_DISABLE":                      "1",
			"NCCL_P2P_DISABLE":                     "0",
			"NCCL_SHM_DISABLE":                     "0",
			"FLASHINFER_DISABLE_VERSION_CHECK":     "1",
			"SGLANG_SKIP_SGL_KERNEL_VERSION_CHECK": "1",
		}
		env["NCCL_NVLS_ENABLE"] = "0"
	}
	// NCCL_NVLS_ENABLE=0 only helps on Hopper (avoids an NVLS init hang under
	// udocker); harmless elsewhere but we only set it when the arch looks Hopper.
	// The agent passes arch via spec.Env when known; default off-switch is safe.
	if len(spec.GPUIndexes) > 0 && !isDomesticSpec(spec) {
		idx := make([]string, len(spec.GPUIndexes))
		for i, g := range spec.GPUIndexes {
			idx[i] = strconv.Itoa(g)
		}
		env["CUDA_VISIBLE_DEVICES"] = strings.Join(idx, ",")
	}
	for k, v := range spec.Env {
		env[k] = v // caller overrides win (e.g. HF_HUB_OFFLINE)
	}
	// Both launchers fill in image Env only for keys absent here, so the
	// default search path above would hide an image's own library directory,
	// which the docker driver (no such default) leaves intact. Put the default
	// in front of the image value instead; a spec value still replaces both,
	// as `docker run -e` does.
	if platform, ok := env[libraryPathEnv]; ok {
		if _, explicit := spec.Env[libraryPathEnv]; !explicit {
			if image, ok := d.imageEnvValue(spec.Name, libraryPathEnv); ok {
				env[libraryPathEnv] = joinSearchPath(platform, image)
			}
		}
	}
	return env
}

// imageEnvValue is key's value in the image Env of container name, as both
// launchers read it: the container.json written when the container was
// created. A container that does not exist yet has no image Env.
func (d *Driver) imageEnvValue(name, key string) (string, bool) {
	if name == "" {
		return "", false
	}
	config, err := runveil.LoadImageConfig(d.containerDir(name))
	if err != nil {
		return "", false
	}
	for _, entry := range config.Env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// joinSearchPath concatenates colon-separated search paths in order, keeping
// the first occurrence of each directory. Empty entries are dropped: the
// loader reads one as the current directory.
func joinSearchPath(paths ...string) string {
	seen := map[string]bool{}
	var dirs []string
	for _, list := range paths {
		for _, dir := range strings.Split(list, ":") {
			key := path.Clean(dir)
			if strings.TrimSpace(dir) == "" || seen[key] {
				continue
			}
			seen[key] = true
			dirs = append(dirs, dir)
		}
	}
	return strings.Join(dirs, ":")
}
