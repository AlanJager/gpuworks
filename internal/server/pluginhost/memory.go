package pluginhost

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// MemoryReader reads how much memory a plugin holds. The RSS watchdog asks
// it about plugins that run outside a cgroup of their own.
type MemoryReader interface {
	// GroupRSS sums the resident set of every process in process group
	// pgid, in bytes. The trampoline starts each plugin in a group of its
	// own, so the sum covers whatever the plugin started.
	GroupRSS(pgid int) (int64, error)
}

// rssLimitFactor: a plugin without a cgroup is restarted once its process
// group holds more than 1.5 times its mem_mib (docs/dev/extensions.md).
const (
	rssLimitNum = 3
	rssLimitDen = 2
)

// rssLimit is the watchdog's threshold for a plugin limited to memMiB.
func rssLimit(memMiB int) int64 { return int64(memMiB) << 20 * rssLimitNum / rssLimitDen }

// memoryWatch is one run's RSS watchdog.
type memoryWatch struct {
	reader MemoryReader
	failed bool // the last read failed; logged once until a read succeeds
}

// checkMemory samples the plugin's process group, records the sample in its
// status and kills the group when it holds more than the watchdog allows.
// A failed read is logged and otherwise ignored: the watchdog never stops
// a plugin it cannot measure.
func (i *instance) checkMemory(proc Process, w *memoryWatch) (outcome, bool) {
	spec := &i.spec
	rss, err := w.reader.GroupRSS(proc.PID())
	if err != nil {
		if !w.failed {
			i.h.log.Warn("plugin memory could not be read", "plugin", spec.ID, "err", err)
		}
		w.failed = true
		return outcome{}, false
	}
	w.failed = false
	i.mu.Lock()
	i.status.RSSBytes = rss
	i.mu.Unlock()
	if rss <= rssLimit(spec.MemMiB) {
		return outcome{}, false
	}
	params := map[string]any{"id": spec.ID, "rss_mib": rss >> 20, "limit_mib": spec.MemMiB}
	i.h.log.Warn("plugin exceeded its memory limit", "plugin", spec.ID, "rss_mib", rss>>20, "limit_mib", spec.MemMiB)
	_ = proc.Signal(syscall.SIGKILL)
	<-proc.Done()
	return outcome{kind: outcomeCrash, code: messages.ErrorPluginMemoryExceeded, params: params}, true
}

// parsePsRSS sums the rss column (KiB) of `ps -A -o pgid=,rss=` output over
// the lines of process group pgid.
func parsePsRSS(out []byte, pgid int) (int64, error) {
	var kib int64
	found := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		g, err1 := strconv.Atoi(f[0])
		r, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 != nil || err2 != nil || g != pgid {
			continue
		}
		kib += r
		found = true
	}
	if !found {
		return 0, fmt.Errorf("no process in group %d", pgid)
	}
	return kib << 10, nil
}

// parseProcStat reads the process group (field 5) and resident pages
// (field 24) from a /proc/<pid>/stat line. The command name (field 2) may
// hold spaces and parentheses, so fields count from its last ')'.
func parseProcStat(raw []byte) (pgrp int, rssPages int64, ok bool) {
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return 0, 0, false
	}
	f := strings.Fields(string(raw[end+1:]))
	// f[0] is field 3 (state): field n is f[n-3].
	if len(f) < 22 {
		return 0, 0, false
	}
	g, err1 := strconv.Atoi(f[2])
	r, err2 := strconv.ParseInt(f[21], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return g, r, true
}
