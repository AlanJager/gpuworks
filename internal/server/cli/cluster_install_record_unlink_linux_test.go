//go:build linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterInstallPrivateReadUnlinkedPendingIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), strings.Repeat("a", 32)+".pending.json")
	if err := os.WriteFile(path, []byte(`{"owned":"pending"}`), 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	// Reader has the exact valid inode open before the publisher completes its
	// canonical pending-marker removal. The old FD now reports Nlink=0.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	raw, err := clusterInstallReadPrivateFile(opened)
	if !errors.Is(err, os.ErrNotExist) || len(raw) != 0 {
		t.Fatalf("completed pending removal classified as invalid record: %q %v", raw, err)
	}
}

func TestClusterInstallPrivateReadRejectsUnlinkedUnsafeOrReplacedRecord(t *testing.T) {
	for _, kind := range []string{"replaced", "public", "empty", "oversized", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "record.json")
			body := []byte(`{"owned":true}`)
			mode := os.FileMode(0600)
			if kind == "public" {
				mode = 0644
			}
			if kind == "empty" {
				body = nil
			}
			if kind == "oversized" {
				body = []byte(strings.Repeat("a", 8193))
			}
			if err := os.WriteFile(path, body, mode); err != nil {
				t.Fatal(err)
			}
			opened, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if kind == "hardlink" {
				if err := os.Link(path, path+".linked"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "replaced" {
					if err := os.WriteFile(path, []byte(`{"replacement":true}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			raw, err := clusterInstallReadPrivateFile(opened)
			if err == nil || errors.Is(err, os.ErrNotExist) || len(raw) != 0 {
				t.Fatalf("unsafe/replaced opened record accepted as absent: %q %v", raw, err)
			}
		})
	}
}

func TestClusterInstallPublisherRemovalKeepsBoundCleanExitReadable(t *testing.T) {
	req := processInstallFixture(t)
	if err := os.Mkdir(req.Dir+"/processes", 0700); err != nil {
		t.Fatal(err)
	}
	boot, err := clusterInstallBootID()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := clusterInstallExecutableHash(req)
	if err != nil {
		t.Fatal(err)
	}
	owner := clusterInstallProcessOwner{clusterInstallPID: clusterInstallPID{999999999, "1"}, Supervisor: clusterInstallPID{999999998, "1"}, Boot: boot, Nonce: strings.Repeat("f", 32), Kind: "join", ExecutableHash: hash}
	if err := clusterInstallPublishOwner(req, owner); err != nil {
		t.Fatal(err)
	}
	pending, err := os.Open(clusterInstallOwnerPath(req, owner.Nonce, "pending"))
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	zero := uint32(0)
	// Invoke the actual durable publisher with the reader retaining its pending
	// inode. Kernel Wait capture itself is covered by the real-child exit tests.
	if err := clusterInstallPublishExit(req, clusterInstallProcessExit{Owner: owner, Status: &zero}, clusterInstallSyncRecordDir); err != nil {
		t.Fatal(err)
	}
	if raw, err := clusterInstallReadPrivateFile(pending); !errors.Is(err, os.ErrNotExist) || len(raw) != 0 {
		t.Fatalf("actual publisher unlink became invalid record: %q %v", raw, err)
	}
	complete, err := clusterInstallReadExit(req, owner)
	if !complete || err != nil {
		t.Fatalf("bound durable clean receipt lost after marker removal: %v %v", complete, err)
	}
}
