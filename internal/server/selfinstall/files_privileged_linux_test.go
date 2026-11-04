//go:build linux

package selfinstall

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrivilegedWriterRejectsDirectoryACLBeforeWriting(t *testing.T) {
	for _, attribute := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		t.Run(attribute, func(t *testing.T) {
			installer, runner, target := privilegedWriteFixture(t, false)
			// A named grant hidden behind read/execute group mode must still be
			// rejected, even though ordinary owner/mode validation would pass.
			entries := []struct {
				tag, perm uint16
				id        uint32
			}{
				{1, 5, ^uint32(0)}, {2, 7, 65534}, {4, 5, ^uint32(0)}, {16, 5, ^uint32(0)}, {32, 0, ^uint32(0)},
			}
			acl := make([]byte, 4+8*len(entries))
			binary.LittleEndian.PutUint32(acl, 2)
			for n, entry := range entries {
				offset := 4 + 8*n
				binary.LittleEndian.PutUint16(acl[offset:], entry.tag)
				binary.LittleEndian.PutUint16(acl[offset+2:], entry.perm)
				binary.LittleEndian.PutUint32(acl[offset+4:], entry.id)
			}
			if err := unix.Setxattr(runner.parent, attribute, acl, 0); err != nil {
				t.Fatal(err)
			}
			err := installer.writePrivileged(context.Background(), target, strings.NewReader("secret"), 6, 0o600)
			if err == nil || !strings.Contains(err.Error(), "repair unexpected ACLs") {
				t.Fatalf("expected actionable ACL rejection, got %v", err)
			}
			if len(runner.commands) != 0 {
				t.Fatal("ACL rejection happened after privileged execution")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("target was exposed: %v", err)
			}
			remaining := make([]byte, len(acl))
			n, err := unix.Getxattr(runner.parent, attribute, remaining)
			if err != nil || !bytes.Equal(acl, remaining[:n]) {
				t.Fatalf("writer changed parent ACL: %v", err)
			}
		})
	}
}
