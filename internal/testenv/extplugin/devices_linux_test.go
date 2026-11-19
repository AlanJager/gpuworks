//go:build linux

package extplugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Applied host setup grants a device directory, not nodes: a node created
// there after the sandbox is applied (a device plugged in later) opens for
// reading and writing, and so does a symlink to it in a directory outside
// the grant, because Landlock checks the inode the path resolves to. A node
// in a sibling directory outside the grant is refused. The test directory
// stands in for /dev: what Landlock checks is the same for a regular file
// as for a device node, apart from ioctl, which the grant also carries.
//
//	go test ./internal/testenv/extplugin/ -run TestDeviceDirGrantsNodesCreatedLater
//
// It requires Landlock; under GPUWORKS_TEST_STRICT=1 a kernel without it
// fails the test instead of skipping it.
func TestDeviceDirGrantsNodesCreatedLater(t *testing.T) {
	requireLandlock(t)
	base := t.TempDir()
	dev, elsewhere, sibling := filepath.Join(base, "dev"), filepath.Join(base, "elsewhere"), filepath.Join(base, "sibling")
	for _, d := range []string{dev, elsewhere, sibling} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	node, link, outside := filepath.Join(dev, "hidraw0"), filepath.Join(elsewhere, "key"), filepath.Join(sibling, "hidraw1")
	h := newHost(t, nil, timing())
	spec := fakeSpec(t, "acme_key", "--probe-devices", strings.Join([]string{node, link, outside}, ","))
	spec.DeviceDirs = []string{dev}
	_ = h.Start(spec)
	waitStatus(t, h, "acme_key", "running", running)
	for _, p := range []string{node, outside} {
		if err := os.WriteFile(p, nil, 0o660); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(node, link); err != nil {
		t.Fatal(err)
	}
	rep := probeReport(t, h, "acme_key", "devices")
	opened, _ := rep["opened"].(map[string]any)
	if opened[node] != "" {
		t.Errorf("a node created in the granted directory after the sandbox: %v, want it opened", opened[node])
	}
	if opened[link] != "" {
		t.Errorf("a symlink outside the grant to that node: %v, want it opened", opened[link])
	}
	if opened[outside] != "EACCES" {
		t.Errorf("a node in a sibling directory outside the grant: %v, want EACCES", opened[outside])
	}
}
