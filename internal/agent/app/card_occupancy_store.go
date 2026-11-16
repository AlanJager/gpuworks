package app

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// cardOccupancyFile keeps, beside the agent's state, the foreign use the
// last reading established, by card identity. A restarted agent finds its
// own instances still on the cards and does not examine those cards, so
// without it the processes outside the platform that a partition was placed
// beside would be forgotten at every upgrade, and the card would read as
// free (ADR 0068). Only this agent reads it; nothing reaches the server
// from it that a reading could not have said.
const cardOccupancyFile = "card-occupancy.json"

// procStat is where Linux gives the running boot's start (btime).
const procStat = "/proc/stat"

// hostBootTime is when the running boot started, zero where it cannot be
// read.
func hostBootTime() time.Time {
	data, err := os.ReadFile(procStat)
	if err != nil {
		return time.Time{}
	}
	return parseBootTime(string(data))
}

// parseBootTime reads the btime line of /proc/stat's content.
func parseBootTime(data string) time.Time {
	for _, line := range strings.Split(data, "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			if secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil && secs > 0 {
				return time.Unix(secs, 0)
			}
		}
	}
	return time.Time{}
}

// load reads what a previous run of this agent knew. A missing or broken
// file is no knowledge: the next reading examines every card nothing of
// ours runs on, as at first start. A PID names a process only within one
// boot - after a reboot our own engine may be given one the file names - so
// a file last written before the running boot started (or when that start
// cannot be read) is kept without its PIDs: the processes are restated at
// the figure they last held, as holders without a PID are, and matched
// against no live process (stillForeign) until the card is examined again.
func (o *cardOccupancy) load(path string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.path = path
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var last map[string][]proto.CardProcess
	if err := json.Unmarshal(data, &last); err != nil {
		slog.Debug("card occupancy file unreadable; starting without it", "path", path, "err", err)
		return
	}
	if o.bootTime == nil {
		o.bootTime = hostBootTime
	}
	if boot := o.bootTime(); boot.IsZero() || info.ModTime().Before(boot) {
		for _, procs := range last {
			for i := range procs {
				procs[i].PID = 0
			}
		}
	}
	o.last = last
}

// saveLocked writes the current answer when which processes hold which card
// has changed since it was last written; a change only in what they hold is
// read again within the minute and is not worth a write. The caller holds
// o.mu. A failed write costs only what a restart would remember.
func (o *cardOccupancy) saveLocked(previous map[string][]proto.CardProcess) {
	if o.path == "" || sameOccupancy(previous, o.last) {
		return
	}
	data, err := json.Marshal(o.last)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(o.path), cardOccupancyFile+".*")
	if err != nil {
		slog.Debug("card occupancy not saved", "err", err)
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), o.path); err != nil {
		_ = os.Remove(tmp.Name())
	}
}

func sameOccupancy(a, b map[string][]proto.CardProcess) bool {
	if len(a) != len(b) {
		return false
	}
	for ident, procs := range a {
		other, ok := b[ident]
		if !ok || !sameProcesses(procs, other) {
			return false
		}
	}
	return true
}
