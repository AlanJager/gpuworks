package app

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The NVIDIA checks only Doctor runs: driver/fabric-manager facts, a CUDA
// driver API smoke, peer access and an NCCL all-reduce. They are explicit and
// manual because the multi-GPU ones take tens of seconds.

// cudaRecheck initialises the CUDA driver the way the smoke does; it prints 0
// when the driver initialises.
const cudaRecheck = `python3 -c 'import ctypes; print(ctypes.CDLL("libcuda.so.1").cuInit(0))'`

func doctorNVIDIAChecks(ctx context.Context, r *proto.DoctorReport, gpuCount int) {
	// The driver version is the one the inventory already reported, not a
	// fresh nvidia-smi: on a host where a card failed to initialize every
	// nvidia-smi waits on the driver lock for up to a minute, and the
	// inventory's answer comes from the agent's shared, cached queries.
	fabric := "driver=" + r.Accelerators.Driver + " " + runDiagnostic(ctx, "sh", "-lc", `set +e
fm="$(systemctl is-active nvidia-fabricmanager 2>/dev/null || systemctl is-active nvidia-fabricmanager.service 2>/dev/null || true)"
if [ -z "$fm" ] || [ "$fm" = "unknown" ]; then fm=not-found; fi
fmver="$(nvidia-fabricmanager --version 2>/dev/null | head -1 || true)"
if [ -z "$fmver" ]; then fmver=not-found; fi
printf 'fabric_manager=%s fabric_manager_version=%s\n' "$fm" "$fmver"`)
	cuda := doctorCUDACheck(ctx)
	peer, nccl := cudaDependentSkip, cudaDependentSkip
	// Peer access and NCCL initialize the same driver the CUDA check just
	// failed to (or could not try): they would only fail the same way, after
	// another wait on a driver that may be slow to say so. With fewer than
	// two cards they say that instead, without touching the driver.
	if strings.HasPrefix(cuda, "PASS") || gpuCount < 2 {
		peer = diagnosticPeerAccess(ctx, gpuCount)
		nccl = diagnosticNCCL(ctx, gpuCount)
	}
	r.Facts = append(r.Facts,
		proto.DoctorFact{Code: proto.DoctorFactFabricManager, Value: fabric},
		proto.DoctorFact{Code: proto.DoctorFactCUDARuntime, Value: cuda},
		proto.DoctorFact{Code: proto.DoctorFactPeerAccess, Value: peer},
		proto.DoctorFact{Code: proto.DoctorFactNCCL, Value: nccl})
	if diagnosticFailed(cuda) {
		// The fabric manager state is a lead, not a verdict: only NVSwitch
		// systems need it, so it is shown next to the failure rather than
		// reported on its own.
		r.Findings = append(r.Findings, proto.DoctorFinding{
			Code: proto.DoctorCUDAInitFailed, Level: proto.DoctorLevelError,
			Probe:    "libcuda.so.1: cuInit, cuDeviceGet, cuCtxCreate, cuMemAlloc",
			Evidence: []proto.DoctorField{{Name: "result", Value: cuda}, {Name: "driver/fabric", Value: fabric}},
			Recheck:  cudaRecheck,
		})
	}
	for _, check := range []struct{ probe, result string }{
		{"libcuda.so.1: cuDeviceCanAccessPeer", peer},
		{"torch.distributed all_reduce (backend nccl)", nccl},
	} {
		if diagnosticFailed(check.result) {
			r.Findings = append(r.Findings, proto.DoctorFinding{
				Code: proto.DoctorMultiGPUCheckFailed, Level: proto.DoctorLevelWarning,
				Probe: check.probe, Evidence: []proto.DoctorField{{Name: "result", Value: check.result}},
			})
		}
	}
}

// cudaDependentSkip is what the multi-GPU checks say when the CUDA check did
// not pass.
const cudaDependentSkip = "SKIP (the CUDA runtime check did not pass)"

func diagnosticFailed(result string) bool { return strings.HasPrefix(result, "FAIL") }

func runDiagnostic(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		if result == "" {
			result = err.Error()
		} else {
			result += ": " + err.Error()
		}
	}
	if result == "" {
		result = "not available"
	}
	return result
}

// doctorCUDACheck is diagnosticCUDA; a test stands in for the host's driver.
var doctorCUDACheck = diagnosticCUDA

func diagnosticCUDA(ctx context.Context) string {
	if reason := gpu.CUDASmokePrerequisite(); reason != "" {
		// Said as a skip, and said for the periodic per-card check too,
		// which needs the same two things: a check that never runs is
		// otherwise silent, and silence reads as a pass.
		return "SKIP (" + reason + "; the periodic per-card runtime check does not run either)"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := gpu.CUDARuntimeSmoke(probeCtx); err != nil {
		return "FAIL: " + err.Error()
	}
	return "PASS cuInit=ok cuDeviceGet=ok cuCtxCreate=ok cuMemAlloc=4096B cuMemFree=ok cuCtxDestroy=ok"
}

func diagnosticPeerAccess(ctx context.Context, count int) string {
	if count < 2 {
		return "SKIP (fewer than 2 GPUs)"
	}
	const script = `import ctypes
lib=ctypes.CDLL("libcuda.so.1")
lib.cuInit.argtypes=[ctypes.c_uint]; lib.cuInit.restype=ctypes.c_int
lib.cuDeviceGetCount.argtypes=[ctypes.POINTER(ctypes.c_int)]; lib.cuDeviceGetCount.restype=ctypes.c_int
lib.cuDeviceGet.argtypes=[ctypes.POINTER(ctypes.c_int),ctypes.c_int]; lib.cuDeviceGet.restype=ctypes.c_int
lib.cuDeviceCanAccessPeer.argtypes=[ctypes.POINTER(ctypes.c_int),ctypes.c_int,ctypes.c_int]; lib.cuDeviceCanAccessPeer.restype=ctypes.c_int
def call(name, fn, *args):
    rc=fn(*args)
    if rc: raise RuntimeError("%s=%d" % (name, rc))
call("cuInit", lib.cuInit, 0)
n=ctypes.c_int(); call("cuDeviceGetCount", lib.cuDeviceGetCount, ctypes.byref(n))
if n.value<2:
    print("SKIP (driver reports fewer than 2 GPUs)"); raise SystemExit(0)
devs=[]
for i in range(n.value):
    d=ctypes.c_int(); call("cuDeviceGet", lib.cuDeviceGet, ctypes.byref(d), i); devs.append(d.value)
pairs=[]
for i,a in enumerate(devs):
    for j,b in enumerate(devs):
        if i==j: continue
        can=ctypes.c_int(); call("cuDeviceCanAccessPeer", lib.cuDeviceCanAccessPeer, ctypes.byref(can), a, b)
        pairs.append("%d->%d:%d" % (i,j,can.value))
print("PASS pairs=" + ",".join(pairs))`
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return runPythonDiagnostic(probeCtx, script)
}

func diagnosticNCCL(ctx context.Context, count int) string {
	if count < 2 {
		return "SKIP (fewer than 2 GPUs)"
	}
	// A node without torch cannot run this check; that is a skip, not a
	// failed check.
	const script = `import os, socket, sys
try:
    import torch
except ImportError:
    print("SKIP (torch is not installed)"); sys.exit(0)
import torch.distributed as dist
import torch.multiprocessing as mp
def worker(rank, world, port):
    os.environ["MASTER_ADDR"]="127.0.0.1"; os.environ["MASTER_PORT"]=str(port)
    torch.cuda.set_device(rank)
    dist.init_process_group("nccl", rank=rank, world_size=world)
    x=torch.ones(1, device="cuda")
    dist.all_reduce(x)
    if abs(float(x.item())-world)>1e-4: raise RuntimeError("all_reduce result=%s" % x.item())
    dist.destroy_process_group()
if __name__ == "__main__":
    n=torch.cuda.device_count()
    if n<2:
        print("SKIP (fewer than 2 GPUs)"); raise SystemExit(0)
    s=socket.socket(); s.bind(("127.0.0.1",0)); port=s.getsockname()[1]; s.close()
    mp.spawn(worker, args=(n,port), nprocs=n, join=True)
    print("PASS world_size=%d all_reduce=ok" % n)`
	probeCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	return runPythonFileDiagnostic(probeCtx, script)
}

func runPythonDiagnostic(ctx context.Context, script string) string {
	out, err := exec.CommandContext(ctx, "python3", "-c", script).CombinedOutput()
	return formatDiagnosticOutput(out, err)
}

func runPythonFileDiagnostic(ctx context.Context, script string) string {
	f, err := os.CreateTemp("", "gpuworks-doctor-*.py")
	if err != nil {
		return "FAIL: cannot create diagnostic script: " + err.Error()
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		return "FAIL: cannot write diagnostic script: " + err.Error()
	}
	if err := f.Close(); err != nil {
		return "FAIL: cannot close diagnostic script: " + err.Error()
	}
	out, err := exec.CommandContext(ctx, "python3", path).CombinedOutput()
	return formatDiagnosticOutput(out, err)
}

func formatDiagnosticOutput(out []byte, err error) string {
	result := strings.TrimSpace(string(out))
	if err != nil {
		if result == "" {
			result = err.Error()
		} else {
			result += ": " + err.Error()
		}
		return "FAIL: " + result
	}
	if result == "" {
		return "FAIL: no diagnostic output"
	}
	return result
}
