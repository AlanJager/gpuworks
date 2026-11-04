//go:build linux

package selfinstall

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// These ACLs include a named account outside the platform group. A zero mask
// hides the file grant behind0600; recursive chmod g+rw would expose it again.
func sharingNamedACL(t *testing.T, path, attribute string, masked bool) []byte {
	t.Helper()
	owner, mask := uint16(7), uint16(5)
	if masked {
		owner, mask = 6, 0
	}
	entries := []struct {
		tag, perm uint16
		id        uint32
	}{
		{1, owner, ^uint32(0)}, {2, 6, 65534}, {4, 0, ^uint32(0)}, {16, mask, ^uint32(0)}, {32, 0, ^uint32(0)},
	}
	acl := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(acl, 2)
	for n, entry := range entries {
		offset := 4 + 8*n
		binary.LittleEndian.PutUint16(acl[offset:], entry.tag)
		binary.LittleEndian.PutUint16(acl[offset+2:], entry.perm)
		binary.LittleEndian.PutUint32(acl[offset+4:], entry.id)
	}
	if err := unix.Setxattr(path, attribute, acl, 0); err != nil {
		t.Fatal(err)
	}
	return sharingReadACL(t, path, attribute)
}

func sharingReadACL(t *testing.T, path, attribute string) []byte {
	t.Helper()
	data := make([]byte, 4096)
	n, err := unix.Getxattr(path, attribute, data)
	if err != nil {
		t.Fatal(err)
	}
	return data[:n]
}

func sharingACLHarness(t *testing.T) *harness {
	t.Helper()
	root := mustEvalSymlinks(t, t.TempDir())
	h := newHarness(t, filepath.Join(root, "install"))
	h.layout.UnitDir = filepath.Join(root, "units")
	h.layout.LaunchdDir = filepath.Join(root, "launchd")
	h.installer = New(h.layout, h.deps, h.io)
	permissions := permissionRunner{root}
	h.run.on = func(cmd Command) {
		switch cmd.Path {
		case "chown", "chmod", "find":
			if err := permissions.Run(context.Background(), cmd); err != nil {
				t.Fatal(err)
			}
		}
	}
	return h
}

func sharingPrepareDirs(t *testing.T, layout Layout) {
	t.Helper()
	for _, path := range []string{layout.Dir, layout.DataDir, layout.LogsDir, layout.BackupsDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChownTreeRejectsMaskedSensitiveACLBeforeAnyCommand(t *testing.T) {
	h := sharingACLHarness(t)
	sharingPrepareDirs(t, h.layout)
	path := filepath.Join(h.layout.DataDir, "master.key")
	writeFile(t, path, "synthetic key", 0o600)
	before := sharingNamedACL(t, path, "system.posix_acl_access", true)
	if err := h.installer.chownTree(context.Background(), DefaultServiceUser); err == nil || !strings.Contains(err.Error(), "ACL") {
		t.Errorf("masked sensitive ACL must fail closed before sharing, got %v", err)
	}
	if len(h.run.calls) != 0 {
		t.Errorf("masked ACL reached command execution: %v", h.run.executed())
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("masked key mode changed before rejection: info=%v err=%v", info, err)
	}
	current := make([]byte, 4096)
	n, err := unix.Getxattr(path, "system.posix_acl_access", current)
	if err != nil || !bytes.Equal(before, current[:n]) {
		t.Errorf("masked key ACL changed before rejection: %v", err)
	}
}

func TestChownTreeRejectsSensitiveDirectoryACLBeforeGrant(t *testing.T) {
	for _, location := range []string{"intermediate", "backups"} {
		for _, attribute := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
			t.Run(location+"/"+attribute, func(t *testing.T) {
				h := sharingACLHarness(t)
				h.layout.DataDir = filepath.Join(h.layout.Dir, "nested", "data")
				h.installer = New(h.layout, h.deps, h.io)
				sharingPrepareDirs(t, h.layout)
				path := filepath.Dir(h.layout.DataDir)
				if location == "backups" {
					path = h.layout.BackupsDir
				}
				before := sharingNamedACL(t, path, attribute, false)
				if err := h.installer.chownTree(context.Background(), DefaultServiceUser); err == nil || !strings.Contains(err.Error(), "ACL") {
					t.Errorf("directory ACL must fail before group sharing: %v", err)
				}
				if len(h.run.calls) != 0 {
					t.Errorf("directory ACL reached command execution: %v", h.run.executed())
				}
				if !bytes.Equal(before, sharingReadACL(t, path, attribute)) {
					t.Error("rejection changed directory ACL")
				}
			})
		}
	}
}

func TestGroupInstallRejectsDefaultACLBeforeWrites(t *testing.T) {
	h := sharingACLHarness(t)
	sharingPrepareDirs(t, h.layout)
	h.installSelf("new synthetic binary")
	before := sharingNamedACL(t, h.layout.DataDir, "system.posix_acl_default", false)
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	req.Password = "sharing-acl-regression-password-2026"
	if _, err := h.installer.Install(context.Background(), req); err == nil || !strings.Contains(err.Error(), "ACL") {
		t.Errorf("default ACL must reject fresh group install: %v", err)
	}
	for _, path := range []string{h.layout.Binary(), h.layout.ConfigPath(), h.layout.UnitPath(), filepath.Join(h.layout.DataDir, "initial-credentials")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("group install published before ACL rejection: %s, %v", path, err)
		}
	}
	for _, cmd := range h.run.calls {
		if cmd.Sudo || cmd.Path == "systemctl" {
			t.Errorf("group install reached privileged/service command before ACL rejection: %s", cmd.String())
		}
	}
	if !bytes.Equal(before, sharingReadACL(t, h.layout.DataDir, "system.posix_acl_default")) {
		t.Error("install rejection changed parent default ACL")
	}
}

func TestNohupInstallKeepsMaskedSensitiveACLPrivate(t *testing.T) {
	h := sharingACLHarness(t)
	sharingPrepareDirs(t, h.layout)
	h.installSelf("new synthetic binary")
	path := filepath.Join(h.layout.DataDir, "master.key")
	writeFile(t, path, "synthetic private key", 0o600)
	before := sharingNamedACL(t, path, "system.posix_acl_access", true)
	req := h.quiet()
	req.NoSystemd = true
	req.Password = "sharing-acl-regression-password-2026"
	if _, err := h.installer.Install(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private install relaxed key mode: info=%v err=%v", info, err)
	}
	if !bytes.Equal(before, sharingReadACL(t, path, "system.posix_acl_access")) {
		t.Error("private owner install changed masked ACL")
	}
	for _, cmd := range h.run.calls {
		if cmd.Path == "chown" || cmd.Path == "chmod" || cmd.Path == "find" || cmd.Path == "groupadd" || cmd.Path == "addgroup" {
			t.Errorf("private install reached group sharing command: %s", cmd.String())
		}
	}
}
