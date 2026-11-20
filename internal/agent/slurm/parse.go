// Package slurm makes an agent on a Slurm compute node a guest of Slurm's
// card allocation: the cards are Slurm's, other jobs' cards are reported as
// held outside the platform, and an instance's cards are borrowed with a
// placeholder job of the agent's own before it starts and returned when no
// instance uses them. It talks to Slurm only through its CLI (scontrol,
// squeue, sbatch, scancel), the same way the agent's runtimes shell out.
package slurm

import (
	"fmt"
	"strconv"
	"strings"
)

// fields splits a one-line scontrol record (`scontrol -o`) into its
// key=value pairs. A key that repeats (the per-node detail of `-d`) keeps
// its first value; detail is read separately by nodeGRES.
func fields(line string) map[string]string {
	out := map[string]string{}
	for _, tok := range strings.Fields(line) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		if _, seen := out[k]; !seen {
			out[k] = v
		}
	}
	return out
}

// nodeGRES is the GRES detail `scontrol -d -o show job` gives for the node
// named node: each "Nodes=<hostlist> CPU_IDs=… Mem=… GRES=<gres>" group
// names the nodes it is about, and only the group covering node counts.
// "" when the job holds no GRES there.
func nodeGRES(line, node string) string {
	toks := strings.Fields(line)
	for i, tok := range toks {
		list, ok := strings.CutPrefix(tok, "Nodes=")
		if !ok || !hostlistHas(list, node) {
			continue
		}
		for _, next := range toks[i+1:] {
			if strings.HasPrefix(next, "Nodes=") {
				break
			}
			if gres, ok := strings.CutPrefix(next, "GRES="); ok {
				return gres
			}
		}
	}
	return ""
}

// gpuIDX is the GPU indexes in a GRES string such as
// "gpu:t4:1(IDX:0)", "gpu:2(IDX:0-1)" or "gpu:a100:3(IDX:0,2-3),nic:1". The
// index is Slurm's position of the device on the node, not a device minor
// (see deviceOrder).
func gpuIDX(gres string) ([]int, error) {
	var out []int
	for _, part := range splitTop(gres) {
		if !strings.HasPrefix(part, "gpu") {
			continue
		}
		_, rest, ok := strings.Cut(part, "(IDX:")
		if !ok {
			continue
		}
		list, _, _ := strings.Cut(rest, ")")
		if list == "N/A" || list == "" {
			continue
		}
		idx, err := rangeList(list)
		if err != nil {
			return nil, fmt.Errorf("parse GRES %q: %w", gres, err)
		}
		out = append(out, idx...)
	}
	return out, nil
}

// splitTop splits on commas outside parentheses.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// rangeList expands "0,2-3" into [0 2 3].
func rangeList(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, err
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return nil, err
			}
		}
		if b < a || b-a > 4096 {
			return nil, fmt.Errorf("bad range %q", part)
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	return out, nil
}

// hostlistHas reports whether a Slurm hostlist ("n1", "n[1-3,7]",
// "gpu-a,gpu-b[01-02]") names node. Zero-padded ranges keep their width.
func hostlistHas(list, node string) bool {
	for _, item := range splitHostlist(list) {
		prefix, rest, ok := strings.Cut(item, "[")
		if !ok {
			if item == node {
				return true
			}
			continue
		}
		body, suffix, _ := strings.Cut(rest, "]")
		if !strings.HasPrefix(node, prefix) || !strings.HasSuffix(node, suffix) {
			continue
		}
		num := strings.TrimSuffix(strings.TrimPrefix(node, prefix), suffix)
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		for _, part := range strings.Split(body, ",") {
			lo, hi, isRange := strings.Cut(part, "-")
			if !isRange {
				hi = lo
			}
			a, errA := strconv.Atoi(lo)
			b, errB := strconv.Atoi(hi)
			if errA != nil || errB != nil {
				continue
			}
			if len(lo) > 1 && lo[0] == '0' && len(num) != len(lo) {
				continue
			}
			if n >= a && n <= b {
				return true
			}
		}
	}
	return false
}

// splitHostlist splits on commas outside brackets.
func splitHostlist(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
