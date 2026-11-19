//go:build linux

package hostidentity

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A default ACL survives creating a 0600 file with its mask disabled. The
// final chmod to 0440 can enable its named reader, so mode bits alone cannot
// prove that only the service group can read raw host facts.
func TestWriteRefusesDirectoryACLsBeforePublishingRawFacts(t *testing.T) {
	for _, attribute := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		t.Run(attribute, func(t *testing.T) {
			dir := runDir(t)
			path := filepath.Join(dir, "host-identity.json")
			old := []byte("old facts\n")
			if err := os.WriteFile(path, old, fileMode); err != nil {
				t.Fatal(err)
			}
			// ACL version 2, then owner, named user, group, mask and other.
			// Access mode remains 0755 despite the extra named reader.
			acl := binary.LittleEndian.AppendUint32(nil, 2)
			for _, entry := range [][3]uint32{{1, 7, ^uint32(0)}, {2, 4, uint32(os.Getuid() + 1)}, {4, 5, ^uint32(0)}, {16, 5, ^uint32(0)}, {32, 5, ^uint32(0)}} {
				acl = binary.LittleEndian.AppendUint16(acl, uint16(entry[0]))
				acl = binary.LittleEndian.AppendUint16(acl, uint16(entry[1]))
				acl = binary.LittleEndian.AppendUint32(acl, entry[2])
			}
			if err := unix.Setxattr(dir, attribute, acl, 0); err != nil {
				if errors.Is(err, unix.EOPNOTSUPP) && os.Getenv("GPUWORKS_TEST_STRICT") != "1" {
					t.Skip("filesystem does not support POSIX ACLs")
				}
				t.Fatal(err)
			}
			if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != dirMode {
				t.Fatalf("ACL fixture changed directory mode: %v, %v", info, err)
			}
			if err := writeAt(self(dir), []byte("new raw facts\n")); err == nil || !strings.Contains(err.Error(), "ACL") {
				t.Fatalf("raw facts published through an ACL: %v", err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != string(old) {
				t.Fatalf("refused write changed prior facts: %q, %v", got, err)
			}
			if names := entries(t, dir); len(names) != 1 || names[0] != filepath.Base(path) {
				t.Fatalf("refused write left files: %v", names)
			}
		})
	}
}
