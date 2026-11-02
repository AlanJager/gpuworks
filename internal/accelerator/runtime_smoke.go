package accelerator

import "strings"

// The per-card runtime checks' own scripts, and the shell commands that
// reproduce one of them by hand.
//
// They live here because the command a person is shown has to be the command
// that was actually run. The agent runs these checks; the server shows them
// for a finding it has only stored. Neither can import the other, so the one
// copy of the script belongs in this leaf package, which both already use for
// the vendor commands a finding carries (HealthGuideFor).
//
// Every command is a whole, pasteable shell snippet: it locates what it needs
// and names the one card it is about. Clients truncate what they display; the
// text itself is never shortened, because a command that cannot be pasted is
// worth less than no command at all.

// AscendToolkitHomes are the host CANN installations the Ascend runtime check
// looks for, in order. An entry containing a wildcard is a glob: the agent
// expands it with filepath.Glob, the reproduction command lets the shell do
// it. ASCEND_HOME_PATH, when set, is tried before all of them.
var AscendToolkitHomes = []string{
	"/usr/local/Ascend/ascend-toolkit/latest",
	"/usr/local/Ascend/cann-*",
}

// AscendACLLibraryPaths are where libascendcl.so sits inside a CANN home.
// Which one it is depends on how the toolkit was installed, so all are tried.
var AscendACLLibraryPaths = []string{
	"lib64/libascendcl.so",
	"aarch64-linux/lib64/libascendcl.so",
	"arm64-linux/lib64/libascendcl.so",
	"x86_64-linux/lib64/libascendcl.so",
}

// AscendACLSmokeScript asks CANN's ACL runtime to take one die, create a
// context on it and allocate 4 KiB. The allocation is the point: it proves a
// context on this die is usable, not merely that npu-smi can print its
// health. ASCEND_RT_VISIBLE_DEVICES leaves the child one die, which it then
// addresses as device zero, so the check cannot drift onto another one.
//
// It uses ctypes so the static agent neither links CANN nor gains a
// build-time dependency on it.
//
// Every failure is graded where it happens. A failure before the die is named
// is the host's and says nothing about this die. A failure while taking the
// die is the die's. A failure while giving it back is neither: the die has
// already answered the question.
const AscendACLSmokeScript = `import ctypes, os


def report(marker, detail, code):
    print("%s: %s" % (marker, detail), flush=True)
    raise SystemExit(code)


def unavailable(detail):
    report("ASCEND_ACL_SMOKE_UNAVAILABLE", detail, 10)


def die_failed(detail):
    report("ASCEND_ACL_SMOKE_DIE_FAILED", detail, 1)


library = os.environ["ASCEND_ACL_LIBRARY"]
try:
    lib = ctypes.CDLL(library)
except OSError as exc:
    unavailable("%s did not load: %s" % (library, exc))

for name, argtypes in (
    ("aclInit", [ctypes.c_char_p]),
    ("aclrtSetDevice", [ctypes.c_int32]),
    ("aclrtCreateContext", [ctypes.POINTER(ctypes.c_void_p), ctypes.c_int32]),
    ("aclrtMalloc", [ctypes.POINTER(ctypes.c_void_p), ctypes.c_size_t, ctypes.c_int]),
    ("aclrtFree", [ctypes.c_void_p]),
    ("aclrtDestroyContext", [ctypes.c_void_p]),
    ("aclrtResetDevice", [ctypes.c_int32]),
    ("aclFinalize", []),
):
    try:
        entry = getattr(lib, name)
    except AttributeError:
        unavailable("%s has no %s" % (library, name))
    entry.argtypes = argtypes
    entry.restype = ctypes.c_int

rc = lib.aclInit(None)
if rc:
    unavailable("aclInit=%d" % rc)
rc = lib.aclrtSetDevice(0)
if rc:
    die_failed("aclrtSetDevice=%d" % rc)
context = ctypes.c_void_p()
rc = lib.aclrtCreateContext(ctypes.byref(context), 0)
if rc:
    die_failed("aclrtCreateContext=%d" % rc)
allocation = ctypes.c_void_p()
rc = lib.aclrtMalloc(ctypes.byref(allocation), 4096, 0)
if rc:
    die_failed("aclrtMalloc=%d" % rc)
for name, call in (
    ("aclrtFree", lambda: lib.aclrtFree(allocation)),
    ("aclrtDestroyContext", lambda: lib.aclrtDestroyContext(context)),
    ("aclrtResetDevice", lambda: lib.aclrtResetDevice(0)),
    ("aclFinalize", lambda: lib.aclFinalize()),
):
    rc = call()
    if rc:
        print("ASCEND_ACL_SMOKE_CLEANUP: %s=%d" % (name, rc), flush=True)
print("ASCEND_ACL_SMOKE_OK", flush=True)`

// CUDASmokeScript creates a context on the first visible device and makes a
// tiny allocation on it. Which device that is depends on what the caller made
// visible through CUDA_VISIBLE_DEVICES.
//
// It uses ctypes rather than cgo or nvcc so the agent stays CGO_ENABLED=0 and
// the check needs no compiler toolchain.
const CUDASmokeScript = `import ctypes
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

// AscendRuntimeSmokeCommand reproduces the Ascend per-die runtime check for
// one die, named by the device index the platform reports it under — the same
// index AscendRuntimeSmokeOn is asked about.
//
// It resolves and locates everything itself, in three steps, so one command
// serves both the agent (which has already resolved these) and a stored
// finding (which has only the index):
//
//   - The logical ACL device id, which is what ASCEND_RT_VISIBLE_DEVICES
//     takes and is not the device index: physical ids can be sparse or
//     remapped by passthrough. `npu-smi info -m` prints the mapping, with a
//     Chip Phy-ID column on multi-chip cards (910C) and none on one-chip
//     cards (910B), where the die index is the NPU id.
//   - The CANN installation, looked for where the agent looks: where
//     libascendcl.so sits depends on the install, and a command naming the
//     wrong path sends the reader after the toolkit instead of the die.
//   - The library path, built rather than appended to, because an empty
//     element in LD_LIBRARY_PATH means the current directory to the dynamic
//     linker.
func AscendRuntimeSmokeCommand(index string) string {
	if strings.TrimSpace(index) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("die=" + ShellSingleQuote(index) + "\n\n")
	b.WriteString("# the logical ACL device id of this die (not its device index)\n")
	b.WriteString(`acl_id="$(npu-smi info -m | awk -v d="$die" '` +
		`NF>=5 && $4==d {print $3; exit} NF==4 && $1==d && $2==0 {print $3; exit}')"` + "\n")
	b.WriteString(`[ -n "$acl_id" ] || { echo "die $die has no logical device mapping"; exit 1; }` + "\n")
	b.WriteString("\n# the host CANN installation, looked for where the agent looks\n")
	b.WriteString(`for home in "${ASCEND_HOME_PATH:-}" ` + strings.Join(AscendToolkitHomes, " ") + "; do\n")
	b.WriteString("  for lib in " + strings.Join(AscendACLLibraryPaths, " ") + "; do\n")
	b.WriteString(`    [ -f "$home/$lib" ] && cann="$home" && acl="$home/$lib" && break 2` + "\n")
	b.WriteString("  done\n")
	b.WriteString("done\n")
	b.WriteString(`[ -n "${acl:-}" ] || { echo "no host CANN installation: the runtime check cannot run here"; exit 1; }` + "\n")
	b.WriteString("\n# take this die alone, create a context on it and allocate 4 KiB\n")
	b.WriteString(AscendEnvRTVisibleDevices + `="$acl_id" \` + "\n")
	b.WriteString(`ASCEND_HOME_PATH="$cann" ASCEND_ACL_LIBRARY="$acl" \` + "\n")
	b.WriteString(`LD_LIBRARY_PATH="$cann/lib64:${acl%/*}:` + AscendDriverLibraryPath + `${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" \` + "\n")
	b.WriteString("python3 -c " + ShellSingleQuote(AscendACLSmokeScript))
	return b.String()
}

// CUDARuntimeSmokeCommand reproduces the NVIDIA per-card runtime check for
// one card, named by UUID the way CUDA_VISIBLE_DEVICES wants it: a UUID
// cannot drift onto another card when the set of working cards changes, the
// way an index can.
func CUDARuntimeSmokeCommand(visible string) string {
	if strings.TrimSpace(visible) == "" {
		return ""
	}
	return EnvCUDAVisibleDevices + "=" + ShellSingleQuote(visible) + " python3 -c " + ShellSingleQuote(CUDASmokeScript)
}

// shellSingleQuote wraps a value for a POSIX shell. The scripts above have no
// single quote in them (TestRuntimeSmokeScriptsSurviveShellQuoting holds them
// to it), but a value interpolated into a command must be safe whatever it
// turns out to be.
// ShellSingleQuote wraps a value for a POSIX shell, so a command carrying it
// can be pasted whatever the value turns out to be.
func ShellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
