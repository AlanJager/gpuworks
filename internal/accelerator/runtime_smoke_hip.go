package accelerator

import "strings"

// HygonHIPLibraryPaths are where a DTK installation keeps the HIP runtime.
var HygonHIPLibraryPaths = []string{
	"/opt/dtk/lib/libamdhip64.so",
	"/opt/dtk/lib64/libamdhip64.so",
	"/opt/rocm/lib/libamdhip64.so",
}

// HygonHIPLibraryDirs are the directories the HIP runtime resolves its own
// dependencies from on a DTK host (the HYHAL driver library among them).
var HygonHIPLibraryDirs = []string{"/opt/dtk/lib", "/opt/dtk/lib64", "/opt/hyhal/lib"}

// HygonHIPSmokeScript asks DTK's HIP runtime to take the card at
// HYGON_SMOKE_BDF and allocate 4 KiB on it, the Hygon counterpart of the CUDA
// and ACL checks (0.2 s on a K100, measured on 172.25.17.133).
//
// The card is found by its PCI address rather than made the only visible one:
// HIP's device order is the runtime's own and need not match hy-smi's, and a
// check that drifted onto another card would blame the wrong one. Nothing is
// created on the other cards.
//
// Every failure is graded where it happens, as the ACL check does: before the
// card is found it is the host's, while taking the card it is the card's,
// and while giving it back it is neither.
const HygonHIPSmokeScript = `import ctypes, os


def report(marker, detail, code):
    print("%s: %s" % (marker, detail), flush=True)
    raise SystemExit(code)


def unavailable(detail):
    report("HYGON_HIP_SMOKE_UNAVAILABLE", detail, 10)


def card_failed(detail):
    report("HYGON_HIP_SMOKE_CARD_FAILED", detail, 1)


def bdf(text):
    domain, _, rest = text.strip().lower().partition(":")
    return domain.lstrip("0").rjust(4, "0") + ":" + rest


library = os.environ["HYGON_HIP_LIBRARY"]
target = bdf(os.environ["HYGON_SMOKE_BDF"])
try:
    lib = ctypes.CDLL(library)
except OSError as exc:
    unavailable("%s did not load: %s" % (library, exc))

for name, argtypes in (
    ("hipInit", [ctypes.c_uint]),
    ("hipGetDeviceCount", [ctypes.POINTER(ctypes.c_int)]),
    ("hipDeviceGetPCIBusId", [ctypes.c_char_p, ctypes.c_int, ctypes.c_int]),
    ("hipSetDevice", [ctypes.c_int]),
    ("hipMalloc", [ctypes.POINTER(ctypes.c_void_p), ctypes.c_size_t]),
    ("hipFree", [ctypes.c_void_p]),
):
    try:
        entry = getattr(lib, name)
    except AttributeError:
        unavailable("%s has no %s" % (library, name))
    entry.argtypes = argtypes
    entry.restype = ctypes.c_int

rc = lib.hipInit(0)
if rc:
    unavailable("hipInit=%d" % rc)
count = ctypes.c_int()
rc = lib.hipGetDeviceCount(ctypes.byref(count))
if rc:
    unavailable("hipGetDeviceCount=%d" % rc)
device = -1
for i in range(count.value):
    buf = ctypes.create_string_buffer(64)
    if lib.hipDeviceGetPCIBusId(buf, 64, i) == 0 and bdf(buf.value.decode()) == target:
        device = i
        break
if device < 0:
    unavailable("HIP lists %d device(s), none at %s" % (count.value, target))
rc = lib.hipSetDevice(device)
if rc:
    card_failed("hipSetDevice=%d" % rc)
allocation = ctypes.c_void_p()
rc = lib.hipMalloc(ctypes.byref(allocation), 4096)
if rc:
    card_failed("hipMalloc=%d" % rc)
rc = lib.hipFree(allocation)
if rc:
    print("HYGON_HIP_SMOKE_CLEANUP: hipFree=%d" % rc, flush=True)
print("HYGON_HIP_SMOKE_OK", flush=True)`

// HygonRuntimeSmokeCommand reproduces the Hygon per-card check for the card at
// bdf, finding the DTK installation where the agent looks for it.
func HygonRuntimeSmokeCommand(bdf string) string {
	bdf = strings.TrimSpace(bdf)
	if bdf == "" {
		return ""
	}
	return "HYGON_SMOKE_BDF=" + ShellSingleQuote(bdf) +
		` HYGON_HIP_LIBRARY="$(ls ` + strings.Join(HygonHIPLibraryPaths, " ") + ` 2>/dev/null | head -n1)"` +
		" LD_LIBRARY_PATH=" + strings.Join(HygonHIPLibraryDirs, ":") +
		" python3 -c " + ShellSingleQuote(HygonHIPSmokeScript)
}
