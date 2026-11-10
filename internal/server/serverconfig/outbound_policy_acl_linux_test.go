//go:build linux

package serverconfig_test

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func TestOutboundPolicyWriterDoesNotGrantInheritedNamedACL(t *testing.T) {
	dir, err := os.MkdirTemp("", "gwks212-review-acl-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "server.yaml")
	original := []byte("ssh_proxy: socks5://operator:review-secret@localhost:1080\n")
	if err := os.WriteFile(path, original, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Getxattr(path, "system.posix_acl_access", nil); !errors.Is(err, unix.ENODATA) {
		t.Fatalf("fixture must start without ACL: %v", err)
	}
	entries := []struct {
		tag, perm uint16
		id        uint32
	}{{1, 7, ^uint32(0)}, {2, 6, 65534}, {4, 7, ^uint32(0)}, {16, 6, ^uint32(0)}, {32, 0, ^uint32(0)}}
	acl := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(acl, 2)
	for i, e := range entries {
		off := 4 + 8*i
		binary.LittleEndian.PutUint16(acl[off:], e.tag)
		binary.LittleEndian.PutUint16(acl[off+2:], e.perm)
		binary.LittleEndian.PutUint32(acl[off+4:], e.id)
	}
	if err := unix.Setxattr(dir, "system.posix_acl_default", acl, 0); err != nil {
		t.Fatal(err)
	}
	_, _, revision, err := serverconfig.ReadOutboundPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	_, saveErr := serverconfig.SaveOutboundPolicy(path, true, []string{"mirror.internal"}, revision)
	if saveErr != nil {
		after, _ := os.ReadFile(path)
		if string(after) != string(original) {
			t.Fatal("failed save altered original file")
		}
		return
	}
	size, err := unix.Getxattr(path, "system.posix_acl_access", nil)
	if errors.Is(err, unix.ENODATA) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, size)
	if _, err := unix.Getxattr(path, "system.posix_acl_access", got); err != nil {
		t.Fatal(err)
	}
	var named, mask uint16
	for off := 4; off+8 <= len(got); off += 8 {
		tag := binary.LittleEndian.Uint16(got[off:])
		perm := binary.LittleEndian.Uint16(got[off+2:])
		if tag == 2 && binary.LittleEndian.Uint32(got[off+4:]) == 65534 {
			named = perm
		}
		if tag == 16 {
			mask = perm
		}
	}
	info, _ := os.Stat(path)
	t.Logf("mode=%#o inherited_named_uid_65534=%d mask=%d effective_read=%v", info.Mode().Perm(), named, mask, named&mask&4 != 0)
	if named&mask&4 != 0 {
		t.Fatal("saving policy granted a previously unauthorized named user read access to server.yaml")
	}
}
