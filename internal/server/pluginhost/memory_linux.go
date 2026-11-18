//go:build linux

package pluginhost

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// procMemory reads /proc: every process's stat names its process group
// and resident pages.
type procMemory struct{ proc string }

func defaultMemoryReader() MemoryReader { return procMemory{proc: "/proc"} }

func (m procMemory) GroupRSS(pgid int) (int64, error) {
	entries, err := os.ReadDir(m.proc)
	if err != nil {
		return 0, err
	}
	var pages int64
	found := false
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		// A process that exited between the listing and the read is
		// simply not counted.
		raw, err := os.ReadFile(filepath.Join(m.proc, e.Name(), "stat"))
		if err != nil {
			continue
		}
		g, rss, ok := parseProcStat(raw)
		if !ok || g != pgid {
			continue
		}
		pages += rss
		found = true
	}
	if !found {
		return 0, fmt.Errorf("no process in group %d", pgid)
	}
	return pages * int64(os.Getpagesize()), nil
}
