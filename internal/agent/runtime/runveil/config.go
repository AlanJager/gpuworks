// Package runveil is GPUWorks' thin replacement for the udocker CLI: it
// launches a workload in a udocker-layout container by invoking the patched
// PRoot directly, without Python.
//
// It reproduces exactly the part of `udocker run` that GPUWorks relies on in
// the P1 execution mode (PRoot with seccomp): image metadata defaults, the
// generated passwd/group pair, the host system bind set, the uid mapping and
// the filtered environment. It reads the UDOCKER_DIR layout that the native
// pull/create pipeline already writes, so the two launchers can operate on the
// same containers and switching between them needs no migration.
//
// The package is a leaf: it only builds argv/env and writes the two small
// account files beside the container. Starting, tracking and stopping the
// process remain the driver's job.
package runveil

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ImageConfig is the subset of the image configuration that `udocker run`
// consults for a container: its container.json is the image config blob.
type ImageConfig struct {
	User       string
	WorkingDir string
	Entrypoint []string
	Cmd        []string
	Env        []string
}

// LoadImageConfig reads containerDir/container.json the way udocker's
// ContainerStructure.get_container_meta does: the "config" section wins over
// "container_config", a key is looked up as spelled and then lowercased, and
// a string where a list is expected is split on whitespace.
func LoadImageConfig(containerDir string) (ImageConfig, error) {
	data, err := os.ReadFile(filepath.Join(containerDir, "container.json"))
	if err != nil {
		return ImageConfig{}, fmt.Errorf("read container metadata: %w", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return ImageConfig{}, fmt.Errorf("decode container metadata: %w", err)
	}
	section := map[string]json.RawMessage{}
	for _, key := range []string{"config", "container_config"} {
		if raw, ok := doc[key]; ok {
			// udocker picks the first present section even when it is null.
			_ = json.Unmarshal(raw, &section)
			break
		}
	}
	lookup := func(key string) json.RawMessage {
		for _, candidate := range []string{key, strings.ToLower(key)} {
			if raw, ok := section[candidate]; ok && !isNull(raw) {
				return raw
			}
			if raw, ok := doc[candidate]; ok && !isNull(raw) {
				return raw
			}
		}
		return nil
	}
	return ImageConfig{
		User:       metaString(lookup("User")),
		WorkingDir: metaString(lookup("WorkingDir")),
		Entrypoint: metaList(lookup("Entrypoint")),
		Cmd:        metaList(lookup("Cmd")),
		Env:        metaList(lookup("Env")),
	}, nil
}

func isNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func metaString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, " ")
	}
	return ""
}

func metaList(raw json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.Fields(s)
	}
	// A map (for example ExposedPorts) is rendered as key:value pairs.
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		out := make([]string, 0, len(m))
		for k, v := range m {
			out = append(out, fmt.Sprintf("%s:%v", k, v))
		}
		sort.Strings(out)
		return out
	}
	return nil
}
