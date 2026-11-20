//go:build linux && dqlite && mounttests

package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Run only in a runner-created private mount namespace. These are actual
// mounted-consumer cross-device links, independent of native Raft and GPUs.
func TestReceivedMaterialRealBindMountEXDEV(t *testing.T) {
	materialMountRequirePrivateNamespace(t)
	for _, fault := range []string{"fallback", "cancel after EXDEV then retry", "corrupt source after EXDEV"} {
		t.Run(fault, func(t *testing.T) {
			body := []byte("original received logo on a separately mounted consumer")
			source, target, digest, name, marker := receivedLogoTestRoots(t, body)
			if err := target.Mkdir("model-logos", 0700); err != nil {
				t.Fatal(err)
			}
			mountpoint := filepath.Join(target.Name(), "model-logos")
			backing := t.TempDir()
			if err := syscall.Mount("tmpfs", backing, "tmpfs", syscall.MS_NODEV|syscall.MS_NOSUID, "size=4m,mode=0700"); err != nil {
				if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENODEV) {
					materialMountUnavailable(t, "tmpfs mount capability unavailable: "+err.Error())
				}
				t.Fatal("mount scoped backing", err)
			}
			materialMountCleanup(t, backing)
			if err := syscall.Mount(backing, mountpoint, "", syscall.MS_BIND, ""); err != nil {
				if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
					materialMountUnavailable(t, "bind mount capability unavailable: "+err.Error())
				}
				t.Fatal("bind scoped backing to consumer", err)
			}
			// LIFO: consumer unmount, backing unmount, then TempDir cleanup.
			materialMountCleanup(t, mountpoint)
			materialMountAssertDevices(t, target, backing, mountpoint)
			foreign := "model-logos/foreign-keep"
			foreignBytes := []byte("unrelated consumer entry must survive")
			if err := target.WriteFile(foreign, foreignBytes, 0600); err != nil {
				t.Fatal(err)
			}
			foreignInfo, err := target.Lstat(foreign)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			links, exdev := 0, 0
			delegate := func(ctx context.Context, root *os.Root, stage, final string) error {
				links++
				stageInfo, err := root.Lstat(stage)
				if err != nil {
					t.Fatal(err)
				}
				err = linkReceivedMaterial(ctx, root, stage, final)
				if errors.Is(err, syscall.EXDEV) {
					exdev++
					if !strings.HasPrefix(stage, materialStageDirectory+"/") || exdev != 1 {
						t.Fatalf("EXDEV did not originate at actual central stage: %q %v", stage, err)
					}
					consumerInfo, statErr := root.Stat("model-logos")
					if statErr != nil || stageInfo.Sys().(*syscall.Stat_t).Dev == consumerInfo.Sys().(*syscall.Stat_t).Dev {
						t.Fatal("actual EXDEV lacked two real device identities", statErr)
					}
					if fault == "cancel after EXDEV then retry" {
						cancel()
					}
					if fault == "corrupt source after EXDEV" {
						if writeErr := source.WriteFile(digest[:2]+"/"+digest, bytes.Repeat([]byte("x"), len(body)), 0600); writeErr != nil {
							t.Fatal(writeErr)
						}
					}
				}
				return err // Always delegate the real link error, never inject EXDEV.
			}
			err = copyReceivedMaterialWithLink(ctx, source, target, name, digest, int64(len(body)), true, delegate)
			if exdev != 1 {
				t.Fatalf("case did not execute one real cross-device link: calls=%d EXDEV=%d err=%v", links, exdev, err)
			}
			switch fault {
			case "fallback":
				if err != nil || links != 2 {
					t.Fatalf("real parent-local fallback: links=%d err=%v", links, err)
				}
				materialMountAssertLogo(t, target, name, marker, body)
			case "cancel after EXDEV then retry":
				if !errors.Is(err, context.Canceled) || links != 1 {
					t.Fatalf("canceled fallback attempted publication: links=%d err=%v", links, err)
				}
				materialMountAssertAbsent(t, target, name, marker)
				materialMountAssertNoStages(t, target)
				// A new context retries the same production entry and mounted
				// consumer. The failed attempt grants no inherited success.
				if err := copyReceivedMaterial(t.Context(), source, target, name, digest, int64(len(body)), true); err != nil {
					t.Fatal("retry after actual EXDEV cancellation", err)
				}
				materialMountAssertLogo(t, target, name, marker, body)
			case "corrupt source after EXDEV":
				if err == nil || links != 1 {
					t.Fatalf("changed source produced false fallback publication: links=%d err=%v", links, err)
				}
				materialMountAssertAbsent(t, target, name, marker)
			}
			materialMountAssertNoStages(t, target)
			current, err := target.Lstat(foreign)
			if err != nil || !os.SameFile(foreignInfo, current) || foreignInfo.Mode() != current.Mode() || !foreignInfo.ModTime().Equal(current.ModTime()) {
				t.Fatal("fallback cleanup changed foreign consumer inode", err)
			}
			actual, err := target.ReadFile(foreign)
			if err != nil || !bytes.Equal(actual, foreignBytes) {
				t.Fatal("fallback cleanup changed foreign consumer bytes", err)
			}
			t.Log("real central-stage link returned kernel EXDEV; only captured parent-local fallback/owned cleanup permitted")
		})
	}
}

func materialMountCleanup(t *testing.T, mountpoint string) {
	t.Helper()
	t.Cleanup(func() {
		if err := syscall.Unmount(mountpoint, 0); err != nil {
			t.Error("normal scoped unmount failed", mountpoint, err)
			if err := syscall.Unmount(mountpoint, syscall.MNT_DETACH); err != nil {
				t.Error("scoped detach failed", mountpoint, err)
			}
		}
	})
}

func materialMountUnavailable(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatal(reason)
	}
	t.Skip(reason)
}

func materialMountRequirePrivateNamespace(t *testing.T) {
	t.Helper()
	if os.Getenv("GPUWORKS_TEST_PRIVATE_MOUNT_NAMESPACE") != "1" || os.Geteuid() != 0 {
		materialMountUnavailable(t, "mounttests require explicit private namespace runner and euid 0")
	}
	parent := os.Getenv("GPUWORKS_TEST_PARENT_MOUNT_NAMESPACE")
	current, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil || parent == "" || !strings.HasPrefix(parent, "mnt:[") || !strings.HasSuffix(parent, "]") || current == parent {
		materialMountUnavailable(t, "mounttests require an actual namespace different from the runner's recorded parent")
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, " shared:") {
			materialMountUnavailable(t, "mounttests require private propagation before any test mount")
		}
	}
}

func materialMountAssertDevices(t *testing.T, root *os.Root, backing, mountpoint string) {
	t.Helper()
	central, err := root.Stat(materialStageDirectory)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := root.Stat("model-logos")
	if err != nil {
		t.Fatal(err)
	}
	if central.Sys().(*syscall.Stat_t).Dev == consumer.Sys().(*syscall.Stat_t).Dev {
		t.Fatal("consumer mount shares the central stage device")
	}
	backingInfo, err := os.Stat(backing)
	if err != nil || !os.SameFile(backingInfo, consumer) {
		t.Fatal("consumer is not the actual mounted backing inode", err)
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 6 && fields[4] == mountpoint && strings.Contains(line, " - tmpfs ") {
			return
		}
	}
	t.Fatal("actual scoped tmpfs consumer absent from mountinfo")
}

func materialMountAssertLogo(t *testing.T, root *os.Root, name, marker string, body []byte) {
	t.Helper()
	actual, err := root.ReadFile(name)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("mounted consumer bytes differ", err)
	}
	final, err := root.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := root.Lstat(marker)
	if err != nil || !os.SameFile(final, origin) {
		t.Fatal("mounted origin marker is not the verified final inode", err)
	}
}

func materialMountAssertAbsent(t *testing.T, root *os.Root, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed fallback published %s: %v", name, err)
		}
	}
}

func materialMountAssertNoStages(t *testing.T, root *os.Root) {
	t.Helper()
	for _, dir := range []string{materialStageDirectory, "model-logos"} {
		entries, err := fs.ReadDir(root.FS(), dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".material-") {
				t.Fatalf("actual fallback left an owned temporary stage in %s: %s", dir, entry.Name())
			}
		}
	}
}
