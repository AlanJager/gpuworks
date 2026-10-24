package deploy

import (
	"encoding/json"
	"strings"
)

// engramSharedMemoryDefaultBytes is the isolated /dev/shm baseline for an
// Engram table shared by local DP ranks. launchShmBytes still adds the CPU KV
// tier and honors explicit sizes and host IPC first.
const engramSharedMemoryDefaultBytes = int64(512) << 30

type engramConfig struct {
	CPUOffload     bool `json:"cpu_offload"`
	DPSharedMemory bool `json:"dp_shared_memory"`
}

// engramSharesHostMemory recognizes the inline JSON config in an effective
// argv. The last valid occurrence wins; malformed values are left to the engine
// and do not count as valid occurrences. Arbitrary manual shell is not parsed
// or guessed here.
func engramSharesHostMemory(args []string) bool {
	found := false
	shared := false
	for i, arg := range args {
		raw := ""
		switch {
		case arg == "--engram-config" && i+1 < len(args):
			raw = args[i+1]
		case strings.HasPrefix(arg, "--engram-config="):
			raw = strings.TrimPrefix(arg, "--engram-config=")
		}
		if raw == "" {
			continue
		}
		var config engramConfig
		if json.Unmarshal([]byte(raw), &config) != nil {
			continue
		}
		found = true
		shared = config.CPUOffload && config.DPSharedMemory
	}
	return found && shared
}

// withEngramSharedMemoryDockerArgs adds only the capability Engram needs.
// Locked-memory ulimits remain owned by launchDockerArgs so all sources share
// one memlock rule. Existing capability spellings are kept.
func withEngramSharedMemoryDockerArgs(args []string, command []string) []string {
	if !engramSharesHostMemory(command) || hasIPCLockCapability(args) {
		return args
	}
	out := append([]string(nil), args...)
	return append(out, "--cap-add", "IPC_LOCK")
}

func hasIPCLockCapability(args []string) bool {
	for index, arg := range args {
		if dockerOptionName(strings.TrimSpace(arg)) == "--cap-add" && strings.EqualFold(dockerOptionValue(args, index), "IPC_LOCK") {
			return true
		}
	}
	return false
}
