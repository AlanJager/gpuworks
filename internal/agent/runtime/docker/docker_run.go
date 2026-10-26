package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Run starts a detached container while retaining its exact creation identity
// for failed-start and abandoned-run cleanup.
func (d *Driver) Run(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	d.runMu.Lock()
	if d.pendingRuns[spec.Name] != nil {
		d.runMu.Unlock()
		return "", fmt.Errorf("Docker container %s has a pending run or cleanup", spec.Name)
	}
	if d.pendingRuns == nil {
		d.pendingRuns = make(map[string]*pendingRun)
	}
	d.pendingRuns[spec.Name] = &pendingRun{}
	d.runMu.Unlock()
	cidDir, err := os.MkdirTemp("", "gpuworks-docker-run-")
	if err != nil {
		d.finishRun(spec.Name, "", nil)
		return "", fmt.Errorf("prepare Docker container ID file: %w", err)
	}
	cidPath := filepath.Join(cidDir, "cid")
	args := buildRunArgs(spec)
	args = append(args[:2], append([]string{"--cidfile", cidPath}, args[2:]...)...)
	return runtime.RunDetached(ctx, runtime.DetachedRun{
		CLI: "docker", Args: args, Name: spec.Name, Remove: d.remove,
		Timeout: runTimeout, AbandonGrace: abandonGrace,
		OnComplete: func(id string, err error) { d.finishRun(spec.Name, id, err) },
		CreatedID: func() (string, error) {
			// RunDetached invokes this only after the CLI has exited, including
			// background timeout cleanup, so Docker can still write a late ID.
			defer os.RemoveAll(cidDir)
			cid, err := os.ReadFile(cidPath)
			if errors.Is(err, os.ErrNotExist) {
				return "", nil
			}
			if err != nil {
				return "", fmt.Errorf("read Docker container ID: %w", err)
			}
			id := strings.TrimSpace(string(cid))
			if id == "" {
				return "", errors.New("Docker recorded an empty container ID")
			}
			return id, nil
		},
	})
}

// Overridden by tests; zero selects the runtime package defaults.
var runTimeout, abandonGrace time.Duration

// pendingRun retains the creation boundary until the CLI and its cleanup are
// finished. An unknown ID after a killed client must remain fail-closed.
type pendingRun struct {
	id         string
	cleanupErr error
}

func (d *Driver) finishRun(name, id string, cleanupErr error) {
	d.runMu.Lock()
	defer d.runMu.Unlock()
	if cleanupErr == nil {
		delete(d.pendingRuns, name)
	} else {
		d.pendingRuns[name] = &pendingRun{id: id, cleanupErr: cleanupErr}
	}
}

func (d *Driver) checkPendingRun(ctx context.Context, name string) error {
	d.runMu.Lock()
	pending := d.pendingRuns[name]
	d.runMu.Unlock()
	if pending == nil {
		return nil
	}
	if pending.cleanupErr == nil {
		return fmt.Errorf("Docker container %s has a pending run; creation has not finished", name)
	}
	if pending.id == "" {
		return fmt.Errorf("Docker container %s creation remains unconfirmed: %w", name, pending.cleanupErr)
	}
	// The CLI has exited, so retry only the exact container that run created.
	if err := d.remove(ctx, pending.id); err != nil {
		return fmt.Errorf("Docker container %s cleanup pending: %w", name, err)
	}
	d.runMu.Lock()
	if d.pendingRuns[name] == pending {
		delete(d.pendingRuns, name)
	}
	d.runMu.Unlock()
	return nil
}
