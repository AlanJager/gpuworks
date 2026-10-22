// Package perftest drives perftest's ib_write_bw to measure RDMA bandwidth
// between two nodes rail by rail. The agent cannot speak verbs itself (it is a
// static, cgo-free binary), so the tool does the measuring and this package
// only locates it (locate.go), starts it, bounds it and parses it.
package perftest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Tool is the perftest binary a node needs for RDMA bandwidth tests.
const Tool = "ib_write_bw"

// Rail is one RDMA port as "device:port".
type Rail struct {
	Device string
	Port   int
}

func (r Rail) String() string { return fmt.Sprintf("%s:%d", r.Device, r.Port) }

// ParseRail reads "mlx5_0:1"; a bare device means port 1.
func ParseRail(s string) (Rail, error) {
	device, port, ok := strings.Cut(strings.TrimSpace(s), ":")
	r := Rail{Device: device, Port: 1}
	if device == "" || strings.ContainsAny(device, " /\\") {
		return Rail{}, fmt.Errorf("invalid rdma port %q", s)
	}
	if ok {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 255 {
			return Rail{}, fmt.Errorf("invalid rdma port %q", s)
		}
		r.Port = n
	}
	return r, nil
}

// Runner starts ib_write_bw. Locate names the binary to run; Start runs it in
// the background (server side) and Output runs it to completion (client
// side). All are injectable.
type Runner struct {
	Locate func() (string, error)
	Output func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
	Start  func(ctx context.Context, name string, args ...string) (wait func() error, err error)
	// SysRoot is the sysfs mount, read to pick a RoCE v2 GID index.
	SysRoot string
}

// Host is the Runner for the local host. The embedded ib_write_bw, used when
// the host has none, is extracted under stateDir.
func Host(stateDir string) Runner {
	loc := newLocator(filepath.Join(stateDir, "tools"))
	return Runner{
		Locate: loc.locate,
		Output: func(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
			var stdout, stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			return stdout.Bytes(), stderr.Bytes(), err
		},
		Start: func(ctx context.Context, name string, args ...string) (func() error, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return cmd.Wait, nil
		},
		SysRoot: "/sys",
	}
}

// Available reports whether the node can run ib_write_bw.
func (r Runner) Available() bool {
	_, err := r.Locate()
	return err == nil
}

// args builds the common per-rail arguments. RoCE ports get an explicit IPv4
// RoCE v2 GID index, the entry NCCL (2.21+) and UCX use; InfiniBand ports do
// not use one. A RoCE port without such a GID fails here rather than letting
// the tool fall back to a link-local or RoCE v1 GID that real traffic never
// takes.
func (r Runner) args(rail Rail, port, seconds int) ([]string, *proto.RDMABenchRail) {
	args := []string{"-d", rail.Device, "-i", strconv.Itoa(rail.Port), "-p", strconv.Itoa(port), "-D", strconv.Itoa(seconds), "--report_gbits", "-F"}
	roce, gid, ok := r.roceV2GID(rail)
	switch {
	case roce && !ok:
		return nil, &proto.RDMABenchRail{Rail: rail.String(), Code: proto.RDMABenchNoRoCEv2GID,
			Error: "RoCE port has no IPv4 RoCE v2 GID (its interface has no IPv4 address)"}
	case roce:
		args = append(args, "-x", strconv.Itoa(gid))
	}
	return args, nil
}

// Serve starts one listening ib_write_bw per rail on ports base, base+1, ...
// Each exits after serving one client, or when ctx ends. A rail that cannot
// be served is returned with its reason and left unserved; the others still
// start.
func (r Runner) Serve(ctx context.Context, rails []Rail, base, seconds int) []proto.RDMABenchRail {
	bin, err := r.Locate()
	if err != nil {
		return allFailed(rails, proto.RDMABenchToolMissing, err.Error())
	}
	var failed []proto.RDMABenchRail
	started := 0
	for i, rail := range rails {
		args, bad := r.args(rail, base+i, seconds)
		if bad != nil {
			failed = append(failed, *bad)
			continue
		}
		wait, err := r.Start(ctx, bin, args...)
		if err != nil {
			failed = append(failed, proto.RDMABenchRail{Rail: rail.String(), Code: proto.RDMABenchToolFailed, Error: fmt.Sprintf("start %s: %v", Tool, err)})
			continue
		}
		started++
		go func() { _ = wait() }()
	}
	if started == 0 {
		return failed
	}
	// ib_write_bw needs a moment to open its TCP handshake socket.
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
	}
	return failed
}

// Run connects rail i to the target's port base+i, all rails at once, and
// returns each rail's average bandwidth or failure.
func (r Runner) Run(ctx context.Context, target string, rails []Rail, base, seconds int) []proto.RDMABenchRail {
	bin, err := r.Locate()
	if err != nil {
		return allFailed(rails, proto.RDMABenchToolMissing, err.Error())
	}
	results := make([]proto.RDMABenchRail, len(rails))
	var wg sync.WaitGroup
	for i, rail := range rails {
		args, bad := r.args(rail, base+i, seconds)
		if bad != nil {
			results[i] = *bad
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			stdout, stderr, err := r.Output(ctx, bin, append(args, target)...)
			res := proto.RDMABenchRail{Rail: rail.String()}
			if gbps, perr := ParseAverage(string(stdout)); perr == nil {
				res.Gbps = gbps
			} else {
				res.Code, res.Error = proto.RDMABenchToolFailed, failure(stdout, stderr, err)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	return results
}

func allFailed(rails []Rail, code, detail string) []proto.RDMABenchRail {
	out := make([]proto.RDMABenchRail, len(rails))
	for i, rail := range rails {
		out[i] = proto.RDMABenchRail{Rail: rail.String(), Code: code, Error: detail}
	}
	return out
}

// failure explains a run that reported no bandwidth. ib_write_bw writes its
// errors to stderr and its report header to buffered stdout, so combined
// output interleaves them unpredictably; stderr is read on its own and
// preferred.
func failure(stdout, stderr []byte, err error) string {
	msg := strings.Join(lastMeaningful(stderr, 3), "; ")
	if msg == "" {
		msg = strings.Join(lastMeaningful(stdout, 1), "")
	}
	switch {
	case msg == "" && err != nil:
		return err.Error()
	case msg == "":
		return "no bandwidth in " + Tool + " output"
	case err != nil:
		return msg + " (" + err.Error() + ")"
	}
	return msg
}

// lastMeaningful returns up to n trailing lines that say something: not
// blank and not a rule of dashes or asterisks.
func lastMeaningful(out []byte, n int) []string {
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.Trim(line, "-* \t") == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// ParseAverage reads the "BW average[Gb/sec]" column of an ib_write_bw report.
func ParseAverage(out string) (float64, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	header := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.Contains(line, "BW average") {
			header = true
			continue
		}
		if !header || line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		v, err := strconv.ParseFloat(fields[3], 64)
		if err != nil {
			continue
		}
		return v, nil
	}
	return 0, errors.New("no bandwidth in " + Tool + " output")
}

// roceV2GID reports whether the rail is a RoCE port and, if so, the lowest
// GID index of type "RoCE v2" that holds an IPv4-mapped address. A port whose
// link layer cannot be read is treated as InfiniBand (no GID argument).
func (r Runner) roceV2GID(rail Rail) (roce bool, index int, ok bool) {
	if r.SysRoot == "" {
		return false, 0, false
	}
	base := filepath.Join(r.SysRoot, "class", "infiniband", rail.Device, "ports", strconv.Itoa(rail.Port))
	layer, err := os.ReadFile(filepath.Join(base, "link_layer"))
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(layer)), "Ethernet") {
		return false, 0, false
	}
	entries, err := os.ReadDir(filepath.Join(base, "gid_attrs", "types"))
	if err != nil {
		return true, 0, false
	}
	var indexes []int
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			indexes = append(indexes, n)
		}
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		typ, err := os.ReadFile(filepath.Join(base, "gid_attrs", "types", strconv.Itoa(i)))
		if err != nil || strings.TrimSpace(string(typ)) != "RoCE v2" {
			continue
		}
		gid, err := os.ReadFile(filepath.Join(base, "gids", strconv.Itoa(i)))
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(gid)), "0000:0000:0000:0000:0000:ffff:") {
			return true, i, true
		}
	}
	return true, 0, false
}
