package slurm

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// Slurm numbers a node's GPUs by their position in its GRES device list
// (gres.conf, or what AutoDetect found), and job records name cards by that
// position (IDX). The list is what `slurmd -G` prints (on standard error), one device per line
// with its device file; without it the positions are taken to follow the
// device minors, which is how Slurm orders /dev/nvidia[0-N] files and what
// AutoDetect=nvml produces. The device minor names a card through the
// driver's /proc entry.

var (
	gresLine = regexp.MustCompile(`Gres Name=gpu\b.*\bFile=(\S+)`)
	nvMinor  = regexp.MustCompile(`^/dev/nvidia(\d+)$`)
)

// deviceOrder is the device minor at each Slurm GPU position, from the
// output of `slurmd -G`. A File given as a range (/dev/nvidia[0-1]) expands
// to its devices in order. nil when the output names no NVIDIA device files.
func deviceOrder(out []byte) []int {
	var minors []int
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		m := gresLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		for _, file := range expandFile(m[1]) {
			mm := nvMinor.FindStringSubmatch(file)
			if mm == nil {
				return nil
			}
			n, _ := strconv.Atoi(mm[1])
			minors = append(minors, n)
		}
	}
	return minors
}

// expandFile expands "/dev/nvidia[0-1,3]" into its device paths.
func expandFile(file string) []string {
	prefix, rest, ok := strings.Cut(file, "[")
	if !ok {
		return []string{file}
	}
	body, suffix, _ := strings.Cut(rest, "]")
	idx, err := rangeList(body)
	if err != nil {
		return []string{file}
	}
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, prefix+strconv.Itoa(i)+suffix)
	}
	return out
}

// minorBDFs reads the PCI address of each NVIDIA device minor from
// <procRoot>/driver/nvidia/gpus/<bdf>/information ("Device Minor: 0").
func minorBDFs(procRoot string) map[int]string {
	out := map[int]string{}
	dirs, _ := filepath.Glob(filepath.Join(procRoot, "driver", "nvidia", "gpus", "*"))
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, "information"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(k) != "Device Minor" {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				out[n] = accelerator.NormalizeBDF(filepath.Base(dir))
			}
		}
	}
	return out
}

// slurmdConfigArgs are the options of the node's running slurmd that say
// where its configuration comes from and which node it is: a configless node
// (E-HPC's, `slurmd -Z --conf "Gres=gpu:1" --conf-server manager:6817`) has
// no slurm.conf of its own, and `slurmd -G` without them cannot tell its
// GRES. nil when no slurmd runs or it was started with none of them.
func slurmdConfigArgs(procRoot string) []string {
	comms, _ := filepath.Glob(filepath.Join(procRoot, "[0-9]*", "comm"))
	for _, comm := range comms {
		name, err := os.ReadFile(comm)
		if err != nil || strings.TrimSpace(string(name)) != "slurmd" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(comm), "cmdline"))
		if err != nil {
			continue
		}
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		var out []string
		for i := 1; i < len(argv); i++ {
			arg := argv[i]
			switch {
			case arg == "-Z":
				out = append(out, arg)
			case arg == "--conf" || arg == "--conf-server" || arg == "-N" || arg == "-f":
				if i+1 < len(argv) {
					out = append(out, arg, argv[i+1])
					i++
				}
			case strings.HasPrefix(arg, "--conf=") || strings.HasPrefix(arg, "--conf-server="):
				out = append(out, arg)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// slurmdListTimeout bounds one `slurmd -G`. A variable so a test need not wait.
var slurmdListTimeout = 20 * time.Second

// positionBDFs is the PCI address at each Slurm GPU position of this node.
func (l *Leaser) positionBDFs(ctx context.Context) map[int]string {
	byMinor := minorBDFs(l.procRoot)
	var order []int
	// A list the health check wrote as root comes first: an agent that is not
	// root cannot ask slurmd on a configless node.
	if file := l.settings().OrderFile; file != "" {
		if raw, err := os.ReadFile(file); err == nil {
			order = deviceOrder(raw)
		}
	}
	var out []byte
	var err error
	// slurmd prints its GRES list on standard error. A configless node's
	// slurmd (started with --conf-server) needs the running daemon's
	// options to find its configuration, and without them some versions
	// (25.05) never give up: they retry the DNS SRV lookup every ten
	// seconds. So a configless node is asked with those options only, every
	// ask is bounded, and the command execs slurmd so that the deadline
	// stops slurmd itself, not only the shell around it.
	ask := func(args ...string) {
		cctx, cancel := context.WithTimeout(ctx, slurmdListTimeout)
		defer cancel()
		cmd := append([]string{"-c", `exec slurmd -G "$@" 2>&1`, "sh"}, args...)
		if out, err = l.run(cctx, "sh", cmd...); err == nil {
			order = deviceOrder(out)
		}
	}
	if len(order) == 0 {
		if args := slurmdConfigArgs(l.procRoot); len(args) > 0 {
			ask(args...)
		} else {
			ask()
		}
	}
	if len(order) == 0 {
		for minor := range byMinor {
			order = append(order, minor)
		}
		sort.Ints(order)
		// One card has one order; with more, a gres.conf listing its
		// devices out of minor order would put jobs on the wrong cards.
		// A configless node gives an agent that is not root no
		// configuration ("Invalid user id").
		if len(order) > 1 {
			l.log.Warn("slurmd did not list this node's GPUs; taking Slurm's GPU positions in device minor order",
				"node", l.node, "slurmd", lastLine(out), "err", err)
		}
	}
	at := make(map[int]string, len(order))
	for pos, minor := range order {
		if bdf, ok := byMinor[minor]; ok {
			at[pos] = bdf
		}
	}
	return at
}

// lastLine is the last non-empty line of a command's output, where slurmd
// puts the error that stopped it.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
