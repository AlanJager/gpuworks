package assets

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
)

// perftestFiles holds perftest's ib_write_bw built from upstream source (see
// build/runtime-assets/perftest/README.md). It links the host's libibverbs and
// librdmacm dynamically, so it runs wherever the RDMA user-space libraries are
// installed even when the perftest package is not. Each agent embeds the
// build for its own architecture (see embed_linux_*.go).

// ibWriteBWAssets maps host architectures to the embedded ib_write_bw.
var ibWriteBWAssets = map[string]string{
	"amd64": "bin/ib_write_bw-x86_64",
	"arm64": "bin/ib_write_bw-arm64",
}

// ExtractIBWriteBW writes the embedded ib_write_bw for this architecture to
// destPath (0755), keeping an intact copy already there, and returns the path.
func ExtractIBWriteBW(destPath string) (string, error) {
	return extractIBWriteBW(goruntime.GOARCH, destPath)
}

func extractIBWriteBW(goarch, destPath string) (string, error) {
	name := ibWriteBWAssets[goarch]
	if name == "" {
		return "", fmt.Errorf("no embedded ib_write_bw for %s", goarch)
	}
	data, err := perftestFiles.ReadFile(name)
	if err != nil {
		return "", err
	}
	if existing, err := os.ReadFile(destPath); err == nil && sha256hex(existing) == sha256hex(data) {
		return destPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".ib_write_bw-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return destPath, os.Rename(tmp.Name(), destPath)
}
