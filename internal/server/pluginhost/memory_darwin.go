//go:build darwin

package pluginhost

import (
	"context"
	"os/exec"
	"time"
)

// psMemory asks ps: macOS has no /proc.
type psMemory struct{}

func defaultMemoryReader() MemoryReader { return psMemory{} }

func (psMemory) GroupRSS(pgid int) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "pgid=,rss=").Output()
	if err != nil {
		return 0, err
	}
	return parsePsRSS(out, pgid)
}
