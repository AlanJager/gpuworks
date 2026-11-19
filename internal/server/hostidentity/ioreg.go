package hostidentity

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/hwident"
)

// ParseIOReg reads the facts macOS gives from the output of
// `ioreg -rd1 -c IOPlatformExpertDevice`: IOPlatformUUID is the system
// UUID and IOPlatformSerialNumber the board serial, in lines such as
//
//	"IOPlatformUUID" = "6A3B1C2D-4E5F-6071-8293-A4B5C6D7E8F9"
//
// macOS has no machine id or root file system UUID of the Linux kind.
func ParseIOReg(out string) Facts {
	f := Facts{V: FileVersion}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch strings.TrimSpace(key) {
		case `"IOPlatformUUID"`:
			f.SystemUUID = Normalize(hwident.KindHost, value)
		case `"IOPlatformSerialNumber"`:
			f.BoardSerial = Normalize(hwident.KindBoardSerial, value)
		}
	}
	return f
}
