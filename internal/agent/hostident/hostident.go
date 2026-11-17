// Package hostident reads the host's firmware system UUID, the one machine
// fact that survives an OS reinstall and a disk swap: SMBIOS/DMI
// product_uuid on Linux, IOPlatformUUID on macOS. The agent reports it in
// its hello (proto.Hello.HostSystemUUID).
package hostident

import (
	"io/fs"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
)

// dmiProductUUID is the SMBIOS system UUID, relative to the file-system
// root. The kernel makes it readable by root only.
const dmiProductUUID = "sys/class/dmi/id/product_uuid"

// SystemUUID is the host's system UUID, normalized
// (proto.NormalizeHostSystemUUID), or "" when it cannot be read: a non-root
// agent on Linux, a machine without SMBIOS, a firmware placeholder. It is
// read once per process; the value does not change while the machine runs.
var SystemUUID = sync.OnceValue(read)

// readDMI reads the Linux system UUID from fsys, rooted at "/".
func readDMI(fsys fs.FS) string {
	data, err := fs.ReadFile(fsys, dmiProductUUID)
	if err != nil {
		return ""
	}
	return proto.NormalizeHostSystemUUID(string(data))
}

// parseIORegPlatformUUID finds IOPlatformUUID in the output of
// `ioreg -rd1 -c IOPlatformExpertDevice`, a line such as
//
//	"IOPlatformUUID" = "6A3B1C2D-4E5F-6071-8293-A4B5C6D7E8F9"
func parseIORegPlatformUUID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != `"IOPlatformUUID"` {
			continue
		}
		return proto.NormalizeHostSystemUUID(strings.Trim(strings.TrimSpace(value), `"`))
	}
	return ""
}
