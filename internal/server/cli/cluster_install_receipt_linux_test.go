//go:build linux

package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestClusterInstallRejectsUnknownOrAlteredTerminalStatus(t *testing.T) {
	for _, name := range []string{"absent", "missing-status", "invalid-status", "wrong-nonce", "wrong-birth", "unknown-key", "symlink", "permissions", "valid-clean"} {
		t.Run(name, func(t *testing.T) {
			req := processInstallFixture(t)
			if err := os.Mkdir(req.Dir+"/processes", 0700); err != nil {
				t.Fatal(err)
			}
			owner := clusterInstallProcessOwner{clusterInstallPID: clusterInstallPID{999999999, "1"}, Supervisor: clusterInstallPID{999999998, "1"}, Boot: "current-fixture-boot", Nonce: strings.Repeat("b", 32), Kind: "serve", ExecutableHash: strings.Repeat("c", 64)}
			zero := uint32(0)
			receipt := clusterInstallProcessExit{Owner: owner, Status: &zero}
			switch name {
			case "missing-status":
				receipt.Status = nil
			case "invalid-status":
				invalid := uint32(0x10000)
				receipt.Status = &invalid
			case "wrong-nonce":
				receipt.Owner.Nonce = strings.Repeat("d", 32)
			case "wrong-birth":
				receipt.Owner.Start = "2"
			}
			raw, _ := json.Marshal(receipt)
			if name == "unknown-key" {
				raw = append(raw[:len(raw)-1], []byte(",\"success\":true}")...)
			}
			path := clusterInstallOwnerPath(req, owner.Nonce, "exit")
			if name != "absent" {
				if name == "symlink" {
					target := req.Dir + "/target"
					if err := os.WriteFile(target, raw, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else {
					mode := os.FileMode(0600)
					if name == "permissions" {
						mode = 0644
					}
					if err := os.WriteFile(path, raw, mode); err != nil {
						t.Fatal(err)
					}
				}
			}
			complete, err := clusterInstallReadExit(req, owner)
			if name == "valid-clean" {
				if !complete || err != nil {
					t.Fatalf("clean wait failed: %v,%v", complete, err)
				}
			} else if complete || err == nil {
				t.Fatalf("unproven terminal exit accepted: %v,%v", complete, err)
			}
		})
	}
}

func TestClusterInstallPublisherRetainsAlteredPendingEvidence(t *testing.T) {
	req := processInstallFixture(t)
	if err := os.Mkdir(req.Dir+"/processes", 0700); err != nil {
		t.Fatal(err)
	}
	boot, _ := clusterInstallBootID()
	hash, _ := clusterInstallExecutableHash(req)
	owner := clusterInstallProcessOwner{clusterInstallPID: clusterInstallPID{999999999, "1"}, Supervisor: clusterInstallPID{999999998, "1"}, Boot: boot, Nonce: strings.Repeat("e", 32), Kind: "serve", ExecutableHash: hash}
	if err := clusterInstallPublishOwner(req, owner); err != nil {
		t.Fatal(err)
	}
	path := clusterInstallOwnerPath(req, owner.Nonce, "pending")
	altered := owner
	altered.Start = "different-birth"
	raw, _ := json.Marshal(altered)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	zero := uint32(0)
	if err := clusterInstallPublishExit(req, clusterInstallProcessExit{Owner: owner, Status: &zero}, clusterInstallSyncRecordDir); err == nil {
		t.Fatal("publisher erased altered pending evidence and certified success")
	}
	if actual, err := os.ReadFile(path); err != nil || string(actual) != string(raw) {
		t.Fatal("altered publication evidence was lost")
	}
	if complete, err := clusterInstallReadExit(req, owner); complete || err == nil {
		t.Fatalf("altered publication accepted: %v,%v", complete, err)
	}
}
