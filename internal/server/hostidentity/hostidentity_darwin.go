//go:build darwin

package hostidentity

import (
	"context"
	"os/exec"
	"time"
)

// local asks IOKit, through ioreg, for the platform UUID and serial
// number: macOS has no file to read them from, and needs no root for
// them. The timeout only keeps a wedged ioreg from holding up a read.
func local() Facts {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ioreg", "-r", "-d", "1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return Facts{V: FileVersion}
	}
	return ParseIOReg(string(out))
}
