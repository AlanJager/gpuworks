package enginelog

import (
	"regexp"
	"strconv"
)

// DeviceUnknown is Cause.Device when the line that showed a dGPU memory
// limit did not name the device.
const DeviceUnknown = -1

// limitTolerance is how far the device capacity an engine prints may be from
// the instance's dGPU memory limit and still be that limit: engines round
// what they print (vLLM to 0.01 GiB, PyTorch to 0.01 of its unit).
func limitTolerance(limitBytes int64) int64 {
	return max(limitBytes/50, 16<<20)
}

// limitShape is one way an engine reports running out of memory that, on an
// instance with a dGPU memory limit, is the limit: the interposer shows the
// engine a device as large as the limit, and where it refused nothing itself
// (or prints nothing when it does) the engine's own error is all the output
// says. Without a limit the same line is a different failure
// (another process holding the card, which is not an out-of-memory; a device
// fault), so these are read only by MatchWithLimit and add nothing to the
// table.
type limitShape struct {
	match *regexp.Regexp
	// read fills the device and what the engine would have held from the
	// line, and reports whether the line is about a device as large as the
	// limit.
	read func(line string, m []string, limitBytes int64, c *Cause) bool
}

var (
	sizeRE = `([0-9]+(?:\.[0-9]+)?) ?(bytes|KiB|MiB|GiB)`
	// vLLM's startup check: "Free memory on device cuda:0 (0.75/1.0 GiB) on
	// startup is less than desired GPU memory utilization (0.9, 0.9 GiB)".
	freeMemoryRE = regexp.MustCompile(`Free memory on device (?:[a-z]+(?::(\d+))?)? ?\(([0-9.]+)/([0-9.]+) GiB\) on startup is less than desired GPU memory utilization \([0-9.]+, ([0-9.]+) GiB\)`)
	// PyTorch's allocator, in the current and the older wording:
	// "CUDA out of memory. Tried to allocate 20.00 MiB. GPU 0 has a total
	// capacity of 1024.00 MiB of which 0 bytes is free." and
	// "... (GPU 0; 1024.00 MiB total capacity; ...; 0 bytes free; ...)".
	torchOOMRE      = regexp.MustCompile(`(?:CUDA|NPU) out of memory`)
	torchDeviceRE   = regexp.MustCompile(`(?:GPU|NPU) (\d+)(?: has|;)`)
	torchCapacityRE = regexp.MustCompile(`total capacity of ` + sizeRE + `|` + sizeRE + ` total capacity`)
	torchTriedRE    = regexp.MustCompile(`Tried to allocate ` + sizeRE)
	torchFreeRE     = regexp.MustCompile(`of which ` + sizeRE + ` is free|` + sizeRE + ` free;`)
	// vLLM's KV cache check: "(0.44 GiB KV cache is needed, which is larger
	// than the available KV cache memory (0.09 GiB)". It names no device and
	// no capacity.
	kvCacheRE = regexp.MustCompile(`\(([0-9.]+) GiB KV cache is needed, which is larger than the available KV cache memory \(([0-9.]+) GiB\)`)
	// torch_npu's error codes are not here. On Ascend the interposer prints
	// its own refusal (the table's signature), so an allocator error without
	// it (error code 300000) was not refused by the limit; and a device that
	// would not set up (507033) under a 1 GiB limit had run the die out of
	// streams, which a process without a limit met on the same die as well.
)

var limitShapes = []limitShape{
	{match: freeMemoryRE, read: func(_ string, m []string, limitBytes int64, c *Cause) bool {
		free, total, desired := gibBytes(m[2]), gibBytes(m[3]), gibBytes(m[4])
		if !nearLimit(total, limitBytes) {
			return false
		}
		if m[1] != "" {
			c.Device, _ = strconv.Atoi(m[1])
		}
		// What is already held inside the limit plus what the engine asked
		// for: the same quantity the interposer's refusal states.
		c.NeededBytes = total - free + desired
		return true
	}},
	{match: torchOOMRE, read: func(line string, _ []string, limitBytes int64, c *Cause) bool {
		capacity, ok := firstSize(torchCapacityRE, line)
		if !ok || !nearLimit(capacity, limitBytes) {
			return false
		}
		if m := torchDeviceRE.FindStringSubmatch(line); m != nil {
			c.Device, _ = strconv.Atoi(m[1])
		}
		tried, hasTried := firstSize(torchTriedRE, line)
		free, hasFree := firstSize(torchFreeRE, line)
		if hasTried && hasFree {
			c.NeededBytes = capacity - free + tried
		}
		return true
	}},
	// The limit sized the device the KV cache did not fit, so it is a cause;
	// whether a larger limit or a shorter context is the fix the line cannot
	// say, so the KV cache cause stays beside it and no amount is stated.
	{match: kvCacheRE, read: func(_ string, m []string, limitBytes int64, _ *Cause) bool {
		// More left for the KV cache than the whole limit: the engine saw a
		// larger device, which the limit did not size.
		return gibBytes(m[2]) <= limitBytes+limitTolerance(limitBytes)
	}},
}

func nearLimit(bytes, limitBytes int64) bool {
	diff := bytes - limitBytes
	if diff < 0 {
		diff = -diff
	}
	return diff <= limitTolerance(limitBytes)
}

func gibBytes(s string) int64 {
	v, _ := strconv.ParseFloat(s, 64)
	return int64(v * (1 << 30))
}

// firstSize reads the first "<number> <unit>" re captured in line. re has
// its size groups in pairs, one pair per alternative.
func firstSize(re *regexp.Regexp, line string) (int64, bool) {
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	for i := 1; i+1 < len(m); i += 2 {
		if m[i] == "" {
			continue
		}
		v, err := strconv.ParseFloat(m[i], 64)
		if err != nil {
			return 0, false
		}
		switch m[i+1] {
		case "KiB":
			v *= 1 << 10
		case "MiB":
			v *= 1 << 20
		case "GiB":
			v *= 1 << 30
		}
		return int64(v), true
	}
	return 0, false
}

// MatchWithLimit is Match for the output of an instance whose dGPU memory
// limit is limitBytes (0: it has none, and this is Match). The interposer's
// own refusal is the limit wherever it prints. Where it does not, an
// engine's report of running out of memory on a device as large as the limit
// is the limit too: it is reported as CodeDGPUMemoryLimit, with the numbers
// the line gives, in place of the out-of-memory cause the same lines show.
// A KV cache that did not fit keeps its own cause after the limit.
func MatchWithLimit(lines []string, limitBytes int64) []Cause {
	causes := Match(lines)
	if limitBytes <= 0 {
		return causes
	}
	for _, c := range causes {
		if c.Code == CodeDGPUMemoryLimit {
			return causes
		}
	}
	limit := Cause{Code: CodeDGPUMemoryLimit, Device: DeviceUnknown, LimitBytes: limitBytes}
	for i, line := range lines {
		for _, shape := range limitShapes {
			m := shape.match.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			// The newest line's numbers stand, as in Match.
			read := Cause{Device: DeviceUnknown}
			if !shape.read(line, m, limitBytes, &read) {
				continue
			}
			limit.Device, limit.NeededBytes = read.Device, read.NeededBytes
			limit.Evidence = append(limit.Evidence, Evidence{Index: i, Line: line})
			break
		}
	}
	if len(limit.Evidence) == 0 {
		return causes
	}
	if len(limit.Evidence) > MaxEvidence {
		limit.Evidence = limit.Evidence[len(limit.Evidence)-MaxEvidence:]
	}
	// Table order: after a missing glibc, before everything else; the
	// out-of-memory the same exhaustion matched is the limit, not a second
	// cause.
	out := make([]Cause, 0, len(causes)+1)
	placed := false
	for _, c := range causes {
		if !placed && c.Code != CodeGLIBCTooOld {
			out, placed = append(out, limit), true
		}
		if c.Code != CodeCUDAOOM {
			out = append(out, c)
		}
	}
	if !placed {
		out = append(out, limit)
	}
	return out
}
