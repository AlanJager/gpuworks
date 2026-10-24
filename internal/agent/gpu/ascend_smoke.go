package gpu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// ascendSmokeTarget is the host CANN installation used to create an ACL
// context. The library can live under lib64 or a target-specific arch
// directory, depending on how the toolkit was installed.
type ascendSmokeTarget struct {
	Home    string
	Library string
}

// These indirections are the test seams: the offline suite never starts a
// process or assumes the test host has CANN unless it builds the real stub.
var (
	ascendSmokeTargetFinder  = findAscendSmokeTarget
	ascendSmokeCommand       = runAscendSmokeCommand
	ascendSmokeLogicalDevice = ascendLogicalDeviceForSmoke
)

// AscendRuntimeSmokeOn asks CANN's ACL runtime to create a context and make a
// small allocation on one physical die. The driver's physical-to-logical
// mapping selects exactly that die even when IDs are sparse or remapped by
// passthrough. ASCEND_RT_VISIBLE_DEVICES then remaps the logical die to zero
// inside the child, so the script always addresses device zero.
func AscendRuntimeSmokeOn(ctx context.Context, physical string) error {
	visible, err := ascendSmokeLogicalDevice(physical)
	if err != nil {
		return &HostRuntimeUnavailableError{Reason: "the Ascend ACL runtime could not identify the die on this host: " + err.Error()}
	}
	target, ok := ascendSmokeTargetFinder()
	if !ok {
		return &HostRuntimeUnavailableError{Reason: "the Ascend ACL runtime could not be used on this host: CANN ACL runtime is not installed on the host"}
	}
	out, err := ascendSmokeCommand(ctx, visible, target)
	return ascendSmokeVerdict(out, err)
}

func ascendLogicalDeviceForSmoke(physical string) (string, error) {
	index, err := strconv.Atoi(physical)
	if err != nil || index < 0 {
		return "", fmt.Errorf("invalid physical die index %q", physical)
	}
	return (&AscendProber{}).rootlessVisibleDevices([]int{index})
}

// AscendCLSmokePrerequisite says why the per-card ACL check cannot run, or
// "". A driver-only host has npu-smi but no libascendcl, so it must keep the
// vendor CLI's verdict rather than be reported as a failed card.
func AscendCLSmokePrerequisite() string {
	if _, err := exec.LookPath("python3"); err != nil {
		return "the Ascend runtime check needs python3, which is not installed on this node"
	}
	if _, ok := ascendSmokeTargetFinder(); !ok {
		return "the Ascend runtime check needs libascendcl.so from a host CANN installation"
	}
	return ""
}

func runAscendSmokeCommand(ctx context.Context, visible string, target ascendSmokeTarget) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "python3", "-c", ascendACLSmokeScript)
	cmd.Env = ascendSmokeEnv(os.Environ(), visible, target)
	cmd.WaitDelay = 5 * time.Second
	return cmd.CombinedOutput()
}

// ascendSmokeEnv is the environment the ACL check runs in. Only the one die
// named by ASCEND_RT_VISIBLE_DEVICES is visible to the child, which then
// addresses it as device zero, so the check cannot drift onto another die.
//
// The library path is built rather than appended to: an empty element in
// LD_LIBRARY_PATH means the current directory to the dynamic linker, and the
// agent runs as root from a directory it does not control, so a trailing
// colon would have the child search there.
func ascendSmokeEnv(base []string, visible string, target ascendSmokeTarget) []string {
	paths := []string{filepath.Join(target.Home, "lib64"), filepath.Dir(target.Library)}
	// libascendcl resolves against the driver's own libraries, which a host
	// whose ld.so.conf does not already name them cannot find. Without them
	// the library fails to load and every die would answer the same way, so
	// the check would read as a host of broken cards.
	paths = append(paths, strings.Split(accelerator.AscendDriverLibraryPath, ":")...)
	for _, entry := range base {
		if name, value, _ := strings.Cut(entry, "="); name == "LD_LIBRARY_PATH" {
			paths = append(paths, strings.Split(value, ":")...)
		}
	}
	seen := make(map[string]struct{}, len(paths))
	kept := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		kept = append(kept, path)
	}
	return environmentWith(base, map[string]string{
		accelerator.AscendEnvRTVisibleDevices: visible,
		"ASCEND_HOME_PATH":                    target.Home,
		"ASCEND_ACL_LIBRARY":                  target.Library,
		"LD_LIBRARY_PATH":                     strings.Join(kept, ":"),
	})
}

func findAscendSmokeTarget() (ascendSmokeTarget, bool) {
	var homes []string
	if home := strings.TrimSpace(os.Getenv("ASCEND_HOME_PATH")); home != "" {
		homes = append(homes, home)
	}
	homes = append(homes, "/usr/local/Ascend/ascend-toolkit/latest")
	if matches, _ := filepath.Glob("/usr/local/Ascend/cann-*"); len(matches) > 0 {
		homes = append(homes, matches...)
	}
	for _, home := range homes {
		for _, relative := range []string{
			"lib64/libascendcl.so",
			"aarch64-linux/lib64/libascendcl.so",
			"arm64-linux/lib64/libascendcl.so",
			"x86_64-linux/lib64/libascendcl.so",
		} {
			library := filepath.Join(home, relative)
			if info, err := os.Stat(library); err == nil && !info.IsDir() {
				return ascendSmokeTarget{Home: home, Library: library}, true
			}
		}
	}
	return ascendSmokeTarget{}, false
}

// The markers the check reports itself with. Only the die marker blames the
// die that was being asked about; the script decides which one a failure
// earns, because only it knows whether the failure came before or after the
// die was named.
const (
	ascendSmokeOKMarker          = "ASCEND_ACL_SMOKE_OK"
	ascendSmokeDieMarker         = "ASCEND_ACL_SMOKE_DIE_FAILED:"
	ascendSmokeUnavailableMarker = "ASCEND_ACL_SMOKE_UNAVAILABLE:"
)

// ascendSmokeVerdict grades what the check printed. A die is only reported as
// unusable when the check says in so many words that it was the die that
// failed: a host whose CANN cannot be loaded or initialized, a crash, or
// output this agent does not recognize is not evidence about the card.
func ascendSmokeVerdict(out []byte, err error) error {
	text := string(out)
	if detail, ok := ascendSmokeMarkerDetail(text, ascendSmokeUnavailableMarker); ok {
		return &HostRuntimeUnavailableError{Reason: "the Ascend ACL runtime could not be used on this host: " + detail}
	}
	if detail, ok := ascendSmokeMarkerDetail(text, ascendSmokeDieMarker); ok {
		return fmt.Errorf("Ascend ACL runtime smoke failed: %s", detail)
	}
	if err == nil && strings.Contains(text, ascendSmokeOKMarker) {
		return nil
	}
	detail := strings.TrimSpace(text)
	if detail == "" && err != nil {
		detail = err.Error()
	}
	if detail == "" {
		detail = "the check printed nothing"
	}
	return &HostRuntimeUnavailableError{
		Reason: "the Ascend ACL runtime check did not reach the die: " + detail,
	}
}

// ascendSmokeMarkerDetail returns what the named marker reported, and whether
// it was there at all. The marker is matched on its own line so a detail that
// quotes another marker cannot be mistaken for one.
func ascendSmokeMarkerDetail(out, marker string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if detail, found := strings.CutPrefix(line, marker); found {
			return strings.TrimSpace(detail), true
		}
	}
	return "", false
}

func environmentWith(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[name]; !replaced {
			out = append(out, entry)
		}
	}
	for name, value := range overrides {
		out = append(out, name+"="+value)
	}
	return out
}

// The check uses ctypes so the static agent does not link CANN and does not
// add a build-time dependency. The allocation proves a context on this die is
// usable, not merely that npu-smi can print its health.
//
// Every failure is graded where it happens. A failure before the die is named
// is the host's and says nothing about this die. A failure while taking the
// die is the die's. A failure while giving it back is neither: the die has
// already answered the question.
const ascendACLSmokeScript = `import ctypes, os


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
print("ASCEND_ACL_SMOKE_OK")`
