package assets

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
)

const PartitionProbeImage = "gpuworks/partition-probe:v1"

// OpenPartitionProbeImage returns the Docker image archive embedded for the
// current agent architecture. Docker accepts the gzip-compressed save stream
// directly. Keeping the archive in the agent makes probing fully offline.
func OpenPartitionProbeImage() (io.ReadCloser, error) {
	path := "bin/runtime-assets/probe/partition-probe-linux-" + runtime.GOARCH + ".tar.gz"
	data, err := files.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("embedded partition probe image for %s: %w", runtime.GOARCH, err)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
