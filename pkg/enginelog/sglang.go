package enginelog

import (
	"regexp"
	"strconv"
	"strings"
)

// SGLang prints its memory figures as "GB" but computes them in units of
// 1<<30 bytes, so GB and GiB read as the same quantity here.
const gib = `\s*Gi?B`

var (
	// sglangStampRE finds the bracket SGLang stamps on each line: a timestamp
	// followed by either the rank tags of the scheduler process ("TP0 EP0") or,
	// under the platform's logging config, its process id ("4242").
	sglangStampRE = regexp.MustCompile(`\[\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?:[.,]\d+)?\s*([^\]]*)\]`)
	sglangRankRE  = regexp.MustCompile(`\b[A-Z_]*[A-Z]P(\d+)\b`)

	sglangServerArgsRE = regexp.MustCompile(`\bserver_args=`)
	sglangLoadEndRE    = regexp.MustCompile(`(?i)Load weight end\b.*?\bmem usage=\s*` + num + gib)
	sglangKVRE         = regexp.MustCompile(`(?i)KV Cache is allocated\b(.*)`)
	sglangKVSizeRE     = regexp.MustCompile(`(?i)\bKV size:\s*` + num + gib)
	sglangKSizeRE      = regexp.MustCompile(`(?i)\bK size:\s*` + num + gib)
	sglangVSizeRE      = regexp.MustCompile(`(?i)\bV size:\s*` + num + gib)
	sglangTokensRE     = regexp.MustCompile(`(?i)#\s*tokens:\s*` + num)
	sglangMambaRE      = regexp.MustCompile(`(?i)Mamba Cache is allocated\b(.*)`)
	sglangStateSizeRE  = regexp.MustCompile(`(?i)\bsize:\s*` + num + gib)
	sglangGraphEndRE   = regexp.MustCompile(`(?i)\bCapture\b.*?\bCUDA graph end\b.*?\bmem usage=\s*` + num + gib)
)

// parseSGLang reads one GPU's share of an SGLang startup.
//
// Every scheduler process (one per GPU) prints its own copy of the summary,
// so only one process is read: the lowest rank when the lines carry rank tags
// (rank 0 on the first node, the node's first rank on the others), else the
// first process that reports. A process can print each kind of line more
// than once — with speculative decoding the draft model loads its own weights,
// allocates its own KV pool and captures its own CUDA graphs — so the sizes
// are summed over the process's lines.
//
// The memory sums are reported only when the text reaches back to the start
// of the launch, the server_args line the launcher prints once before any
// worker starts: the agent reads a bounded tail, and a tail that has lost the
// start of the summary would give a partial sum that looks whole (a lost
// target-model line next to a kept draft-model line, say). The token count is
// one figure per pool, never summed, so it stands on its own.
func parseSGLang(logText string) Measured {
	var (
		m           Measured
		weights     float64
		kv          float64
		graph       float64
		largestPool float64
		started     bool
		lines       []string
		key         string
		bestRank    = -1
	)
	for _, line := range strings.FieldsFunc(logText, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if sglangServerArgsRE.MatchString(line) {
			started = true
			continue
		}
		if !sglangRelevant(line) {
			continue
		}
		lineKey, rank := sglangProcess(line)
		if bestRank < 0 || rank < bestRank {
			key, bestRank = lineKey, rank
		}
		lines = append(lines, line)
	}
	for _, line := range lines {
		if lineKey, _ := sglangProcess(line); lineKey != key {
			continue
		}
		switch {
		case sglangLoadEndRE.MatchString(line):
			weights += sglangFloat(sglangLoadEndRE, line)
		case sglangKVRE.MatchString(line):
			rest := sglangKVRE.FindStringSubmatch(line)[1]
			size := sglangFloat(sglangKVSizeRE, rest)
			if size == 0 {
				size = sglangFloat(sglangKSizeRE, rest) + sglangFloat(sglangVSizeRE, rest)
			}
			kv += size
			if tokens, ok := sglangInt(sglangTokensRE, rest); ok && (m.KVCacheTokens == 0 || size > largestPool) {
				m.KVCacheTokens = tokens
				largestPool = size
			}
		case sglangMambaRE.MatchString(line):
			// A hybrid model's recurrent state is carved out of the same budget
			// as its KV pool, so it is counted with the cache, not left over.
			for _, match := range sglangStateSizeRE.FindAllStringSubmatch(sglangMambaRE.FindStringSubmatch(line)[1], -1) {
				if v, ok := toFloat(match[1]); ok {
					kv += v
				}
			}
		case sglangGraphEndRE.MatchString(line):
			graph += sglangFloat(sglangGraphEndRE, line)
		}
	}
	if started {
		m.ModelWeightsGiB, m.KVCacheGiB, m.GraphCaptureGiB = weights, kv, graph
	}
	m.Found = m.ModelWeightsGiB != 0 || m.KVCacheGiB != 0 || m.GraphCaptureGiB != 0 || m.KVCacheTokens != 0
	return m
}

func sglangRelevant(line string) bool {
	for _, re := range []*regexp.Regexp{sglangLoadEndRE, sglangKVRE, sglangMambaRE, sglangGraphEndRE} {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// sglangProcess names the process that printed a line, with the sum of its
// rank numbers; a line without rank tags counts as rank 0.
func sglangProcess(line string) (string, int) {
	stamp := sglangStampRE.FindStringSubmatch(line)
	if stamp == nil {
		return "", 0
	}
	tag := strings.TrimSpace(stamp[1])
	sum := 0
	for _, rank := range sglangRankRE.FindAllStringSubmatch(tag, -1) {
		n, _ := strconv.Atoi(rank[1])
		sum += n
	}
	return tag, sum
}

func sglangFloat(re *regexp.Regexp, s string) float64 {
	match := re.FindStringSubmatch(s)
	if match == nil {
		return 0
	}
	v, _ := toFloat(match[1])
	return v
}

func sglangInt(re *regexp.Regexp, s string) (int, bool) {
	match := re.FindStringSubmatch(s)
	if match == nil {
		return 0, false
	}
	v, ok := toInt(match[1])
	return v, ok && v != 0
}
