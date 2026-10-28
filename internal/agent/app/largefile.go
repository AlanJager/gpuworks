package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// StreamLargeFile writes one bounded regular file to the server. The server
// names an absolute path — it derives it from the model directory it recorded
// for this node — and gets either that file's bytes or an error; there is no
// directory listing, no glob and no relative-path resolution here, so this
// stays a narrow operation rather than a second general-purpose file reader.
func (n *NodeRuntime) StreamLargeFile(ctx context.Context, path string, w io.Writer) error {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("large file read requires an absolute path: %s", path)
	}
	if gpu.SimulatesHost(n.prober) && !insideDir(n.stateDir, path) {
		return fmt.Errorf("%s outside the node's directory: %w", path, errSimulatedHost)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("large file read requires a regular file: %s", path)
	}
	if info.Size() > proto.LargeFileReadMaxBytes {
		return fmt.Errorf("file exceeds the %d byte limit: %s", proto.LargeFileReadMaxBytes, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := f.Read(buf)
		if read > 0 {
			if _, err := w.Write(buf[:read]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
