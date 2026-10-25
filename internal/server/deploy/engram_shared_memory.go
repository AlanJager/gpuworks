package deploy

import (
	"encoding/json"
	"strings"
)

// engramSharedMemoryDefaultBytes is the isolated /dev/shm baseline for an
// Engram table shared by local DP ranks. launchShmBytes still adds the CPU KV
// tier and honors explicit sizes and host IPC first.
const engramSharedMemoryDefaultBytes = int64(512) << 30

// engramFlag is the engine option that configures Engram.
const engramFlag = "--engram-config"

type engramValues struct {
	absent         bool
	cpuOffload     bool
	dpSharedMemory bool
}

type engramArgumentState struct {
	present   bool
	shared    bool
	uncertain bool
}

func defaultEngramValues() engramValues {
	return engramValues{cpuOffload: true}
}

// inspectEngramArguments mirrors FlexibleArgumentParser in the fixed vLLM
// release: underscores are normalized in the option root, dotted values are
// merged in argument order, and the resulting object is appended after every
// raw occurrence. A dotted group therefore replaces, rather than merges with,
// the raw JSON object regardless of where the raw option appeared.
func inspectEngramArguments(args []string) engramArgumentState {
	var state engramArgumentState
	var raw engramValues
	rawSet := false
	dotted := map[string]any{}
	dottedSet := false
	invalid := false
	for i := 0; i < len(args); i++ {
		_, field, value, inline, ok := engramArgument(args[i])
		if !ok {
			continue
		}
		state.present = true
		if !inline {
			if i+1 >= len(args) {
				invalid = true
				continue
			}
			i++
			value = args[i]
		}
		if field == "" {
			parsed, valid := parseRawEngramValues(value)
			if !valid {
				invalid = true
				continue
			}
			raw, rawSet = parsed, true
			continue
		}
		dottedSet = true
		var parsed any
		if json.Unmarshal([]byte(value), &parsed) != nil {
			parsed = value
		}
		dotted[field] = parsed
	}

	values := raw
	if !rawSet {
		values.absent = true
	}
	if dottedSet {
		values = defaultEngramValues()
		for field, value := range dotted {
			valid := applyEngramValue(&values, field, value)
			if !valid {
				invalid = true
			}
		}
	}
	state.uncertain = invalid
	state.shared = !values.absent && values.cpuOffload && values.dpSharedMemory && !invalid
	return state
}

func engramArgument(arg string) (root, field, value string, inline, ok bool) {
	name, value, inline := strings.Cut(arg, "=")
	if !strings.HasPrefix(name, "--") {
		return "", "", "", false, false
	}
	root = name
	if before, after, found := strings.Cut(name, "."); found {
		root, field = before, after
	}
	root = strings.ReplaceAll(root, "_", "-")
	if root != engramFlag {
		return "", "", "", false, false
	}
	return root, field, value, inline, true
}

func parseRawEngramValues(raw string) (engramValues, bool) {
	if strings.TrimSpace(raw) == "null" {
		return engramValues{absent: true}, true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || fields == nil {
		return engramValues{}, false
	}
	values := defaultEngramValues()
	for _, field := range []string{"cpu_offload", "dp_shared_memory"} {
		rawValue, found := fields[field]
		if !found {
			continue
		}
		var value any
		if json.Unmarshal(rawValue, &value) != nil || !applyEngramValue(&values, field, value) {
			return engramValues{}, false
		}
	}
	return values, true
}

func applyEngramValue(values *engramValues, field string, value any) bool {
	parsed, ok := engramBool(value)
	if !ok {
		return field != "cpu_offload" && field != "dp_shared_memory"
	}
	switch field {
	case "cpu_offload":
		values.cpuOffload = parsed
	case "dp_shared_memory":
		values.dpSharedMemory = parsed
	}
	return true
}

func engramBool(value any) (bool, bool) {
	switch value := value.(type) {
	case bool:
		return value, true
	case float64:
		if value == 0 {
			return false, true
		}
		if value == 1 {
			return true, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "t", "yes", "y", "on":
			return true, true
		case "0", "false", "f", "no", "n", "off":
			return false, true
		}
	}
	return false, false
}

// engramSharesHostMemory reports only a configuration that can be proven to
// share host memory. launchWarnings separately flags uncertain configurations
// on runtimes that use the host's /dev/shm directly.
func engramSharesHostMemory(args []string) bool {
	return inspectEngramArguments(args).shared
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
