package accelerator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The commands are shown to be pasted, so they are held to being shell: a
// command that does not parse is worse than none, because the reader spends
// their time on the command instead of the card.
func TestRuntimeSmokeCommandsAreValidShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell to parse with")
	}
	for name, command := range map[string]string{
		"ascend": AscendRuntimeSmokeCommand("0"),
		"cuda":   CUDARuntimeSmokeCommand("GPU-6b2f8a1c"),
	} {
		if command == "" {
			t.Fatalf("%s: no command", name)
		}
		// -n parses without running: these commands talk to the accelerator.
		out, err := exec.Command(sh, "-n", "-c", command).CombinedOutput()
		if err != nil {
			t.Errorf("%s does not parse as shell: %v\n%s\n%s", name, err, out, command)
		}
	}
}

// The python the commands carry is the python the checks run. Were they to
// drift, the reader would be told to run something other than what failed the
// card, which is the whole reason these live in one place.
func TestRuntimeSmokeCommandsCarryTheCheckScripts(t *testing.T) {
	ascend := AscendRuntimeSmokeCommand("3")
	if !strings.Contains(ascend, AscendACLSmokeScript) {
		t.Error("the Ascend command does not carry AscendACLSmokeScript")
	}
	if !strings.Contains(ascend, "die='3'") {
		t.Errorf("the Ascend command does not name the die: %s", ascend)
	}
	// ASCEND_RT_VISIBLE_DEVICES takes the logical ACL id, which the command
	// resolves; naming the device index there would address another die on a
	// host whose ids are remapped.
	if !strings.Contains(ascend, AscendEnvRTVisibleDevices+`="$acl_id"`) {
		t.Errorf("the Ascend command does not pass the resolved logical id: %s", ascend)
	}
	if strings.Contains(ascend, AscendEnvRTVisibleDevices+"=3") {
		t.Error("the Ascend command passes the device index as the logical id")
	}
	// The driver's own libraries, without which libascendcl resolves nothing
	// on a host whose ld.so.conf does not name them.
	if !strings.Contains(ascend, AscendDriverLibraryPath) {
		t.Errorf("the Ascend command does not add the driver libraries: %s", ascend)
	}

	cuda := CUDARuntimeSmokeCommand("GPU-6b2f8a1c")
	if !strings.Contains(cuda, CUDASmokeScript) {
		t.Error("the CUDA command does not carry CUDASmokeScript")
	}
	// A UUID, not an index: an index addresses a different card once the
	// driver drops one.
	if !strings.Contains(cuda, EnvCUDAVisibleDevices+"='GPU-6b2f8a1c'") {
		t.Errorf("the CUDA command does not name the card by UUID: %s", cuda)
	}

	if AscendRuntimeSmokeCommand(" ") != "" || CUDARuntimeSmokeCommand("") != "" {
		t.Error("a command was produced for a card that cannot be named")
	}
}

// The scripts are pasted inside single quotes, so a single quote in one would
// end the quoting and hand the shell the rest of the script as commands.
func TestRuntimeSmokeScriptsSurviveShellQuoting(t *testing.T) {
	for name, script := range map[string]string{"ascend": AscendACLSmokeScript, "cuda": CUDASmokeScript} {
		if strings.Contains(script, "'") {
			t.Errorf("%s script contains a single quote; keep it to double quotes so the command can be pasted", name)
		}
	}
	// Whatever a value turns out to be, the command stays one command.
	if got := ShellSingleQuote(`a'b`); got != `'a'\''b'` {
		t.Errorf("ShellSingleQuote(%q) = %s", `a'b`, got)
	}
}

// The awk step renders the driver's physical-to-logical mapping, which has
// two shapes: multi-chip cards (910C) print a Chip Phy-ID column and the die
// index is that Phy-ID; one-chip cards (910B) print no such column and the
// die index is the NPU id. Both are parsed from real `npu-smi info -m`
// output, because picking the wrong column picks the wrong die.
func TestAscendCommandResolvesBothMappingShapes(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk to resolve with")
	}
	// 192.168.200.29: 7 cards, 2 dies each, Phy-ID == logical id.
	const multiChip = `	NPU ID                         Chip ID                        Chip Logic ID                  Chip Phy-ID                    Chip Name
	-1                             0                              -                              -                              Mcu
	0                              0                              0                              0                              Ascend910
	0                              1                              1                              1                              Ascend910
	0                              2                              -                              -                              Mcu
	1                              0                              2                              2                              Ascend910
	1                              1                              3                              3                              Ascend910
`
	// A VM that compacts the ids it passes through: die 4 is logical 1.
	const multiChipRemapped = `	NPU ID  Chip ID  Chip Logic ID  Chip Phy-ID  Chip Name
	2       0        0              2            Ascend910
	2       1        1              4            Ascend910
`
	// 910B4: one chip per card, no Phy-ID column.
	const oneChip = `	NPU ID  Chip ID  Chip Logic ID  Chip Name
	2       0        0              Ascend910B4
	4       0        1              Ascend910B4
	4       1        -              Mcu
`
	for _, tc := range []struct{ name, mapping, die, want string }{
		{"multi-chip die 0", multiChip, "0", "0"},
		{"multi-chip die 3", multiChip, "3", "3"},
		{"multi-chip remapped die 4", multiChipRemapped, "4", "1"},
		{"one chip per card", oneChip, "4", "1"},
		{"absent die", multiChip, "9", ""},
		// An Mcu row names no die and must never be answered with.
		{"mcu row", oneChip, "-1", ""},
	} {
		got := resolveAscendLogicalID(t, tc.mapping, tc.die)
		if got != tc.want {
			t.Errorf("%s: die %s resolved to %q, want %q", tc.name, tc.die, got, tc.want)
		}
	}
}

// resolveAscendLogicalID runs the command's own awk step against mapping
// output, so the test exercises the text that ships rather than a copy.
func resolveAscendLogicalID(t *testing.T, mapping, die string) string {
	t.Helper()
	command := AscendRuntimeSmokeCommand(die)
	_, rest, found := strings.Cut(command, "acl_id=\"$(")
	if !found {
		t.Fatalf("the command has no logical-id step: %s", command)
	}
	step, _, found := strings.Cut(rest, ")\"")
	if !found {
		t.Fatalf("the logical-id step does not end: %s", rest)
	}
	// The step runs npu-smi. A stand-in on PATH stands for it on a host with
	// no Ascend card — a shell function cannot, because "npu-smi" is not a
	// POSIX function name.
	dir := t.TempDir()
	stub := filepath.Join(dir, "npu-smi")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s' \"$MAPPING\"\n"), 0o755); err != nil {
		t.Fatalf("write npu-smi stand-in: %v", err)
	}
	cmd := exec.Command("sh", "-c", "die="+ShellSingleQuote(die)+"\n"+step)
	cmd.Env = append(cmd.Environ(), "MAPPING="+mapping, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the logical-id step failed: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// A card the runtime could not use is the one finding whose check is this
// platform's own rather than the vendor tool's, so every vendor that has a
// host runtime check gets its command, and the ones that do not are left to
// their CLI's verdict.
func TestRuntimeUnusableGuidePerVendor(t *testing.T) {
	nvidia, ok := HealthGuideFor(HealthCard{
		Vendor: VendorNVIDIA, Code: HealthRuntimeUnusable,
		BDF: "00000000:65:00.0", Index: 1, UUID: "GPU-6b2f8a1c",
	}, nil)
	if !ok || !strings.Contains(nvidia.Probe, CUDASmokeScript) {
		t.Fatalf("NVIDIA runtime_unusable guide = %+v", nvidia)
	}
	if nvidia.Recheck != "nvidia-smi -q -i 00000000:65:00.0" {
		t.Errorf("NVIDIA recheck = %q", nvidia.Recheck)
	}
	// Without the UUID the command could only name an index, which addresses
	// another card once the driver drops one.
	if g, ok := HealthGuideFor(HealthCard{Vendor: VendorNVIDIA, Code: HealthRuntimeUnusable, BDF: "00000000:65:00.0", Index: 1}, nil); ok {
		t.Errorf("NVIDIA guide without a UUID = %+v, want none", g)
	}

	ascend, ok := HealthGuideFor(HealthCard{
		Vendor: VendorAscend, Code: HealthRuntimeUnusable, BDF: "0000:9d:00.0", Index: 0,
	}, nil)
	if !ok || !strings.Contains(ascend.Probe, AscendACLSmokeScript) || !strings.Contains(ascend.Probe, "die='0'") {
		t.Fatalf("Ascend runtime_unusable guide = %+v", ascend)
	}
	if !strings.Contains(ascend.Recheck, "npu-smi info") {
		t.Errorf("Ascend recheck = %q", ascend.Recheck)
	}
	// A die the vendor tool never enumerated cannot be named.
	if g, ok := HealthGuideFor(HealthCard{Vendor: VendorAscend, Code: HealthRuntimeUnusable, BDF: "0000:9d:00.0", Index: -1}, nil); ok {
		t.Errorf("Ascend guide for an unenumerated die = %+v, want none", g)
	}

	// Hygon and PPU have no host runtime check; their cards keep the verdict
	// their CLI gave them, and inventing a command would send the reader
	// after a check this platform never made.
	for _, vendor := range []string{VendorHygon, VendorPPU} {
		if g, ok := HealthGuideFor(HealthCard{Vendor: vendor, Code: HealthRuntimeUnusable, BDF: "0000:7e:00.0", Index: 0}, nil); ok {
			t.Errorf("%s runtime_unusable guide = %+v, want none", vendor, g)
		}
	}
}
