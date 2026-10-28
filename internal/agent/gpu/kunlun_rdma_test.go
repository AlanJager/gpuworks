package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withKunlunXDR overrides only the transport path; native device checks still
// exercise the same fixture, and every other unknown path remains an error.
func withKunlunXDR(p *KunlunProber, mode os.FileMode) {
	inspect := p.stat
	p.stat = func(path string) (os.FileInfo, error) {
		if path == "/dev/xdrdrv" {
			return kunlunDeviceInfo{mode: mode}, nil
		}
		return inspect(path)
	}
}

func TestKunlunXDRRequiresRealCharacterDevice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   os.FileMode
		reject bool
	}{
		{"character device", os.ModeDevice | os.ModeCharDevice, false},
		{"regular file", 0600, true},
		{"block device", os.ModeDevice, true},
		{"symlink", os.ModeSymlink, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := kunlunFixture(t)
			withKunlunXDR(p, tc.mode)
			if err := p.verifyKunlunXDR(); (err != nil) != tc.reject {
				t.Fatalf("XDR inspection: %v", err)
			}
		})
	}
	p, _ := kunlunFixture(t)
	if err := p.verifyKunlunXDR(); err != nil {
		t.Fatalf("missing XDR should allow RDMA fallback: %v", err)
	}
}

func TestKunlunResolvedNetworkPreflight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		reject bool
	}{
		{"selected RDMA", []string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"}, false},
		{"finite lock", []string{"--device", "/dev/infiniband", "--ulimit", "memlock=65536"}, true},
		{"missing lock", []string{"--device", "/dev/infiniband"}, true},
		{"conflicting lock", []string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--ulimit", "memlock=65536"}, true},
		{"missing capability", []string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1"}, true},
		{"only one verbs node", []string{"--device", "/dev/infiniband/uverbs0", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"}, true},
		{"TCP", []string{"--network", "host"}, true},
		{"unlimited equals form", []string{"--device=/dev/infiniband", "--ulimit=memlock=-1:-1", "--cap-add=IPC_LOCK"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (resolvedKunlunRDMAArgs(tc.args) != nil) != tc.reject {
				t.Fatal(tc.args)
			}
		})
	}
}
func TestKunlunDeviceInspectorDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	file, link := filepath.Join(root, "file"), filepath.Join(root, "link")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	p := &KunlunProber{}
	info, err := p.inspect(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("device inspector followed symlink: %v", err)
	}
}

func TestKunlunLaunchInjectionIsAtomic(t *testing.T) {
	p, _ := kunlunFixture(t)
	if got := p.RuntimeInjectionForLaunch(context.Background(), nil, "docker", 2, nil); got.UnsupportedReason != "" || len(got.DockerArgs) != 0 {
		t.Fatalf("empty selection should expose nothing: %+v", got)
	}
	// Single-node selected cards do not require a distributed driver.
	if got := p.RuntimeInjectionForLaunch(context.Background(), []int{1}, "docker", 1, nil); got.UnsupportedReason != "" {
		t.Fatal(got)
	}
	if got := p.RuntimeInjectionForLaunch(context.Background(), []int{1}, "docker", 2, []string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"}); got.UnsupportedReason == "" || len(got.DockerArgs) != 0 {
		t.Fatalf("partial injection: %+v", got)
	}
	withKunlunXDR(p, os.ModeDevice|os.ModeCharDevice)
	got := p.RuntimeInjectionForLaunch(context.Background(), []int{1}, "docker", 2, []string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"})
	args := strings.Join(got.DockerArgs, " ")
	if got.UnsupportedReason != "" || !strings.Contains(args, "/dev/xpu1") || !strings.Contains(args, "/dev/xdrdrv") || strings.Contains(args, "/dev/xpu0") {
		t.Fatal(got)
	}
}

func TestKunlunInjectionCanceledDuringXDRValidation(t *testing.T) {
	p, _ := kunlunFixture(t)
	withKunlunXDR(p, os.ModeDevice|os.ModeCharDevice)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inspect := p.stat
	p.stat = func(path string) (os.FileInfo, error) {
		info, err := inspect(path)
		if path == "/dev/xdrdrv" {
			cancel()
		}
		return info, err
	}
	injection := p.RuntimeInjectionForLaunch(ctx, []int{1}, "docker", 2, []string{
		"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK",
	})
	if !strings.Contains(injection.UnsupportedReason, "context canceled") || len(injection.DockerArgs) != 0 {
		t.Fatalf("canceled XDR validation exposed devices: %+v", injection)
	}
}
