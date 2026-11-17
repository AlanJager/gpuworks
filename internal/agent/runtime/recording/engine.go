package recording

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/engines"
)

// WithEngine makes the driver start the fake engine binary (test/fake-engine)
// for every serving container it runs, on the API port the container's
// command names and with the model it mounts, so a process test's instances
// pass their health checks and answer through the gateway. A headless rank
// (port 0, or no port) gets none. The engine is stopped with the container.
func (d *Driver) WithEngine(bin string) *Driver {
	d.mu.Lock()
	d.engineBin = bin
	d.mu.Unlock()
	return d
}

var (
	portFlag   = regexp.MustCompile(`--port[= ]+'?([0-9]+)`)
	servedFlag = regexp.MustCompile(`--served-model-name[= ]+'?([^'\s]+)`)
)

// startEngineLocked starts the fake engine for a container. The command is
// read as text: a manual command is one shell script, an engine command an
// argv, and both name the port and served name as flags.
func (d *Driver) startEngineLocked(id string, spec runtime.WorkloadSpec) error {
	command := strings.Join(append(append([]string(nil), spec.Entrypoint...), spec.Cmd...), " ")
	match := portFlag.FindStringSubmatch(command)
	if match == nil || match[1] == "0" {
		return nil
	}
	model := ""
	for _, mount := range spec.Mounts {
		if _, err := os.Stat(filepath.Join(mount.Source, "config.json")); err == nil {
			model = mount.Source
			break
		}
	}
	if model == "" {
		return fmt.Errorf("recording runtime: %s mounts no model directory for the fake engine", spec.Name)
	}
	args := []string{"--dialect", engines.SGLang, "launch_server", "--model-path", model, "--port", match[1]}
	if served := servedFlag.FindStringSubmatch(command); served != nil {
		args = append(args, "--served-model-name", served[1])
	}
	d.stopEngineLocked(id)
	cmd := exec.Command(d.engineBin, args...)
	if dir := filepath.Dir(d.path); dir != "" {
		log, err := os.Create(filepath.Join(dir, "engine-"+spec.Name+".log"))
		if err == nil {
			cmd.Stdout, cmd.Stderr = log, log
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("recording runtime: start fake engine: %w", err)
	}
	d.engines[id] = cmd
	go func() { _ = cmd.Wait() }()
	return nil
}

func (d *Driver) stopEngineLocked(id string) {
	if cmd := d.engines[id]; cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	delete(d.engines, id)
}
