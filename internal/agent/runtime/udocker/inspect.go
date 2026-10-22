package udocker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// Container states as operator tooling reports them. A container is
// "created" until this node starts it; after that its pid file decides.
const (
	ContainerCreated = "created"
	ContainerRunning = "running"
	ContainerExited  = "exited"
)

// ContainerSummary is one container as `gpuworks-agent runveil ps` shows it,
// derived entirely from UDOCKER_DIR and the driver's state directory.
type ContainerSummary struct {
	Name    string
	Image   string
	State   string
	PID     int // 0 when the node never started the container
	Created time.Time
}

// Containers summarizes every container, stopped ones included. It reads
// state only and never provisions, so it is safe beside a running agent.
func (d *Driver) Containers(ctx context.Context) ([]ContainerSummary, error) {
	names, err := d.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ContainerSummary, 0, len(names))
	for _, name := range names {
		dir := filepath.Join(d.udockerDir, "containers", name)
		summary := ContainerSummary{Name: name, State: ContainerCreated}
		if data, err := os.ReadFile(filepath.Join(dir, "imagerepo.name")); err == nil {
			summary.Image = strings.TrimSpace(string(data))
		}
		// container.json is written once, when the container is created.
		if info, err := os.Stat(filepath.Join(dir, "container.json")); err == nil {
			summary.Created = info.ModTime()
		}
		if pid, ok := d.pidOf(name); ok {
			summary.PID = pid
			summary.State = ContainerExited
			if processAlive(pid) {
				summary.State = ContainerRunning
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

// ImageSummary is one image as `gpuworks-agent runveil images` shows it.
type ImageSummary struct {
	Reference string
	// ID is the manifest digest; empty for an image only the udocker CLI
	// registered, which keeps no manifest.
	ID string
	// Size is the compressed size of config and layers, what the node stores.
	Size int64
}

// Images summarizes every registered image. Like Containers it only reads.
func (d *Driver) Images() ([]ImageSummary, error) {
	layout := native.Layout{Dir: d.udockerDir}
	refs, err := layout.Images()
	if err != nil {
		return nil, err
	}
	out := make([]ImageSummary, 0, len(refs))
	for _, ref := range refs {
		summary := ImageSummary{Reference: ref.String()}
		if manifest, err := layout.LoadManifest(ref); err == nil {
			summary.ID, _ = layout.ImageID(ref)
			summary.Size = manifest.Config.Size
			for _, layer := range manifest.Layers {
				summary.Size += layer.Size
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

// LogPath is the file a started container's output is redirected to.
func (d *Driver) LogPath(name string) string {
	return filepath.Join(d.stateDir, name+".log")
}

// SpecPath is the workload spec the agent records when it starts a container;
// the agent writes it as itself with mode 0600, so its owner is the agent's
// user.
func (d *Driver) SpecPath(name string) string {
	return filepath.Join(d.stateDir, name+".spec.json")
}

// StateDirs reports UDOCKER_DIR and the driver's state directory.
func (d *Driver) StateDirs() (udockerDir, stateDir string) {
	return d.udockerDir, d.stateDir
}
