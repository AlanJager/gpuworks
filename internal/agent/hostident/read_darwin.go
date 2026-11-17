//go:build darwin

package hostident

import (
	"context"
	"os/exec"
	"time"
)

// read asks IOKit, through ioreg, for the platform UUID; there is no file to
// read it from on macOS. ioreg answers in milliseconds, the timeout only
// keeps a wedged one from holding up the hello.
func read() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ioreg", "-r", "-d", "1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	return parseIORegPlatformUUID(string(out))
}
