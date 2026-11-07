package udocker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// ImportArchive registers the docker-save or OCI archive at path (plain or
// gzipped) under image, for operator tooling beside a running agent. Unlike
// LoadImageFile it never provisions: it writes into the native layout the
// agent already set up, and gives what it adds the owner of UDOCKER_DIR, so
// the run user can read and later replace it. The agent's next presence check
// then reports the image the same way it does after a console import.
func (d *Driver) ImportArchive(ctx context.Context, image, path string) error {
	image = strings.TrimSpace(image)
	if imageRepository(image) == "" {
		return fmt.Errorf("an image reference is required")
	}
	if strings.Contains(image, "@") {
		return fmt.Errorf("digest image references are not supported: use the tagged reference the image catalog shows")
	}
	info, err := os.Stat(d.udockerDir)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open image archive: %w", err)
	}
	defer file.Close()
	var owner *native.Owner
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		owner = &native.Owner{UID: int(stat.Uid), GID: int(stat.Gid)}
	}
	return native.NewEngine(d.udockerDir, owner).Load(ctx, image, file)
}
