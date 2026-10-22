package gpu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cudaSmokeCommand runs the check, with only one card visible to it when
// visible is set.
//
// WaitDelay bounds the wait after the context ends: a process stuck in the
// driver ignores the kill until it leaves the kernel, and without it the
// caller would wait as long as the driver does.
var cudaSmokeCommand = func(ctx context.Context, visible string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if visible != "" {
		cmd.Env = append(os.Environ(), "CUDA_VISIBLE_DEVICES="+visible)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd.CombinedOutput()
}

// CUDARuntimeSmokeOn asks whether the runtime can use one card, named as
// CUDA_VISIBLE_DEVICES wants it (a UUID, so it cannot drift onto another
// card the way an index does).
//
// Per card rather than once per node, because the answers differ: a card
// can be the only one a driver failed to bring up, and asking about
// "device 0" says nothing about the rest. It is also cheap — 0.3 s on a
// working card, including one whose memory is 99% spoken for by a running
// engine (measured on gpu3).
func CUDARuntimeSmokeOn(ctx context.Context, visible string) error {
	out, err := cudaSmokeCommand(ctx, visible, "python3", "-c", cudaSmokeScript)
	return cudaSmokeVerdict(out, err)
}

// CUDARuntimeSmoke performs a bounded, dependency-light CUDA driver check.
// It intentionally uses Python ctypes instead of cgo or nvcc so the agent
// remains CGO_ENABLED=0 and the check does not depend on a compiler toolchain.
func CUDARuntimeSmoke(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := cudaSmokeCommand(probeCtx, "", "python3", "-c", cudaSmokeScript)
	return cudaSmokeVerdict(out, err)
}

// cudaSmokeScript creates a context on the first visible device and makes a
// tiny allocation on it. Which device that is depends on what the caller
// made visible.
const cudaSmokeScript = `import ctypes
lib=ctypes.CDLL("libcuda.so.1")
for n in ("cuInit", "cuDeviceGet", "cuCtxCreate_v2", "cuMemAlloc_v2", "cuMemFree_v2", "cuCtxDestroy_v2"):
    if not hasattr(lib, n): raise RuntimeError("missing " + n)
lib.cuInit.argtypes=[ctypes.c_uint]; lib.cuInit.restype=ctypes.c_int
lib.cuDeviceGet.argtypes=[ctypes.POINTER(ctypes.c_int),ctypes.c_int]; lib.cuDeviceGet.restype=ctypes.c_int
lib.cuCtxCreate_v2.argtypes=[ctypes.POINTER(ctypes.c_void_p),ctypes.c_uint,ctypes.c_int]; lib.cuCtxCreate_v2.restype=ctypes.c_int
lib.cuMemAlloc_v2.argtypes=[ctypes.POINTER(ctypes.c_uint64),ctypes.c_size_t]; lib.cuMemAlloc_v2.restype=ctypes.c_int
lib.cuMemFree_v2.argtypes=[ctypes.c_uint64]; lib.cuMemFree_v2.restype=ctypes.c_int
lib.cuCtxDestroy_v2.argtypes=[ctypes.c_void_p]; lib.cuCtxDestroy_v2.restype=ctypes.c_int
def call(name, fn, *args):
    rc=fn(*args)
    if rc: raise RuntimeError("%s=%d" % (name, rc))
call("cuInit", lib.cuInit, 0)
dev=ctypes.c_int(); call("cuDeviceGet", lib.cuDeviceGet, ctypes.byref(dev), 0)
ctx=ctypes.c_void_p(); call("cuCtxCreate", lib.cuCtxCreate_v2, ctypes.byref(ctx), 0, dev)
ptr=ctypes.c_uint64(); call("cuMemAlloc", lib.cuMemAlloc_v2, ctypes.byref(ptr), 4096)
call("cuMemFree", lib.cuMemFree_v2, ptr.value); call("cuCtxDestroy", lib.cuCtxDestroy_v2, ctx)
print("CUDA_RUNTIME_SMOKE_OK")`

// cudaSmokeVerdict reads the attempt's output. It is shared so a per-card
// check and the whole-node one cannot disagree about what counts as a pass.
func cudaSmokeVerdict(out []byte, err error) error {
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("cuda runtime smoke failed: %s", detail)
	}
	if !strings.Contains(string(out), "CUDA_RUNTIME_SMOKE_OK") {
		return fmt.Errorf("cuda runtime smoke returned no success marker: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// CUDASmokePrerequisite says why the CUDA runtime check cannot run on this
// host, or "". The check needs python3 and the driver's user-space library;
// a trimmed host or one whose driver lives in a container has neither in
// the usual place. Saying so is the honest answer — a check that cannot run
// is not a card that failed.
func CUDASmokePrerequisite() string {
	if _, err := exec.LookPath("python3"); err != nil {
		return "the runtime check needs python3, which is not installed on this node"
	}
	for _, path := range cudaLibraryPaths {
		if _, err := os.Stat(path); err == nil {
			return ""
		}
	}
	return "the CUDA driver library (libcuda.so.1) is not where the runtime check can load it"
}

// cudaLibraryPaths are where the driver's user-space library is found. A
// host running the driver from a container (the GPU Operator's pattern)
// keeps it under /run/nvidia/driver rather than in the system path.
var cudaLibraryPaths = []string{
	"/usr/lib/x86_64-linux-gnu/libcuda.so.1",
	"/usr/lib64/libcuda.so.1",
	"/usr/lib/aarch64-linux-gnu/libcuda.so.1",
	"/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.1",
	"/run/nvidia/driver/usr/lib64/libcuda.so.1",
}
