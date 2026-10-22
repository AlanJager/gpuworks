package perftest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// report is ib_write_bw's --report_gbits client output (perftest 4.5).
const report = `---------------------------------------------------------------------------------------
                    RDMA_Write BW Test
 Dual-port       : OFF		Device         : mlx5_0
 Number of qps   : 1		Transport type : IB
---------------------------------------------------------------------------------------
 #bytes     #iterations    BW peak[Gb/sec]    BW average[Gb/sec]   MsgRate[Mpps]
 65536      3354100          0.00               358.74		   0.684249
---------------------------------------------------------------------------------------
`

func TestParseAverage(t *testing.T) {
	if got, err := ParseAverage(report); err != nil || got != 358.74 {
		t.Fatalf("ParseAverage = %v, %v", got, err)
	}
	for _, bad := range []string{"", "Couldn't connect to 10.96.0.48:18515\n", " #bytes #iterations BW peak[Gb/sec] BW average[Gb/sec]\n x y z w\n"} {
		if _, err := ParseAverage(bad); err == nil {
			t.Fatalf("ParseAverage(%q) succeeded", bad)
		}
	}
}

func TestParseRail(t *testing.T) {
	if r, err := ParseRail("mlx5_3:1"); err != nil || r != (Rail{Device: "mlx5_3", Port: 1}) {
		t.Fatalf("%v %v", r, err)
	}
	if r, err := ParseRail("mlx5_bond_0"); err != nil || r.Port != 1 {
		t.Fatalf("%v %v", r, err)
	}
	for _, bad := range []string{"", ":1", "mlx5_0:x", "mlx5_0:0", "a/b:1"} {
		if _, err := ParseRail(bad); err == nil {
			t.Fatalf("ParseRail(%q) succeeded", bad)
		}
	}
}

func roceSysfs(t *testing.T) string {
	root := t.TempDir()
	base := filepath.Join(root, "class/infiniband/mlx5_2/ports/1")
	for path, content := range map[string]string{
		"link_layer":         "Ethernet",
		"gid_attrs/types/0":  "IB/RoCE v1",
		"gid_attrs/types/1":  "RoCE v2",
		"gids/1":             "fe80:0000:0000:0000:2e9d:90ff:fe92:aaac",
		"gid_attrs/types/3":  "RoCE v2",
		"gids/3":             "0000:0000:0000:0000:0000:ffff:ac1e:086f",
		"gid_attrs/types/10": "RoCE v2",
		"gids/10":            "0000:0000:0000:0000:0000:ffff:0a00:0001",
	} {
		p := filepath.Join(base, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ib := filepath.Join(root, "class/infiniband/mlx5_0/ports/1/link_layer")
	os.MkdirAll(filepath.Dir(ib), 0o755)
	os.WriteFile(ib, []byte("InfiniBand\n"), 0o644)
	return root
}

type recorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recorder) add(name string, args []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
}

func (r *recorder) lines() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := map[string]bool{}
	for _, c := range r.calls {
		got[strings.Join(c, " ")] = true
	}
	return got
}

// refusedQP is what ib_write_bw 5.96 prints when a RoCE QP cannot reach the
// peer: the error on stderr, the report header (dashes last) on stdout.
const (
	refusedStdout = `---------------------------------------------------------------------------------------
                    RDMA_Write BW Test
 Link type       : Ethernet
 GID index       : 1
---------------------------------------------------------------------------------------
`
	refusedStderr = "\nFailed to modify QP 92 to RTR\n Unable to Connect the HCA's through the link\n"
)

func TestServeAndRunBuildPerRailCommands(t *testing.T) {
	rec := &recorder{}
	r := Runner{
		Locate: func() (string, error) { return "/usr/bin/ib_write_bw", nil },
		Start: func(_ context.Context, name string, args ...string) (func() error, error) {
			rec.add(name, args)
			return func() error { return nil }, nil
		},
		Output: func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
			rec.add(name, args)
			if args[1] == "mlx5_2" {
				return []byte(refusedStdout), []byte(refusedStderr), errors.New("exit status 1")
			}
			return []byte(report), nil, nil
		},
		SysRoot: roceSysfs(t),
	}
	rails := []Rail{{Device: "mlx5_0", Port: 1}, {Device: "mlx5_2", Port: 1}}
	if failed := r.Serve(context.Background(), rails, 18515, 5); len(failed) != 0 {
		t.Fatalf("serve failed rails = %#v", failed)
	}
	results := r.Run(context.Background(), "10.0.0.2", rails, 18515, 5)
	want := []proto.RDMABenchRail{
		{Rail: "mlx5_0:1", Gbps: 358.74},
		// The real reason comes from stderr, not the dashes that end stdout.
		{Rail: "mlx5_2:1", Code: proto.RDMABenchToolFailed, Error: "Failed to modify QP 92 to RTR; Unable to Connect the HCA's through the link (exit status 1)"},
	}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("results = %#v", results)
	}
	got := rec.lines()
	for _, line := range []string{
		"/usr/bin/ib_write_bw -d mlx5_0 -i 1 -p 18515 -D 5 --report_gbits -F",
		// The RoCE port takes the first IPv4 RoCE v2 GID.
		"/usr/bin/ib_write_bw -d mlx5_2 -i 1 -p 18516 -D 5 --report_gbits -F -x 3",
		"/usr/bin/ib_write_bw -d mlx5_0 -i 1 -p 18515 -D 5 --report_gbits -F 10.0.0.2",
		"/usr/bin/ib_write_bw -d mlx5_2 -i 1 -p 18516 -D 5 --report_gbits -F -x 3 10.0.0.2",
	} {
		if !got[line] {
			t.Fatalf("missing command %q in %v", line, got)
		}
	}
}

// A RoCE port whose interface lost its IPv4 address keeps only link-local
// GIDs. Such a rail fails before the tool runs, on both sides, while the
// other rails still run.
func TestRoCEPortWithoutIPv4GIDFailsItsRailOnly(t *testing.T) {
	root := roceSysfs(t)
	bare := filepath.Join(root, "class/infiniband/hns_0/ports/1")
	for path, content := range map[string]string{
		"link_layer":        "Ethernet",
		"gid_attrs/types/0": "IB/RoCE v1",
		"gids/0":            "fe80:0000:0000:0000:b24f:a6ff:fe70:e261",
		"gid_attrs/types/1": "RoCE v2",
		"gids/1":            "fe80:0000:0000:0000:b24f:a6ff:fe70:e261",
	} {
		p := filepath.Join(bare, path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content+"\n"), 0o644)
	}
	rec := &recorder{}
	r := Runner{
		Locate: func() (string, error) { return "/usr/bin/ib_write_bw", nil },
		Start: func(_ context.Context, name string, args ...string) (func() error, error) {
			rec.add(name, args)
			return func() error { return nil }, nil
		},
		Output: func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
			rec.add(name, args)
			return []byte(report), nil, nil
		},
		SysRoot: root,
	}
	rails := []Rail{{Device: "hns_0", Port: 1}, {Device: "mlx5_2", Port: 1}}
	failed := r.Serve(context.Background(), rails, 18515, 3)
	if len(failed) != 1 || failed[0].Rail != "hns_0:1" || failed[0].Code != proto.RDMABenchNoRoCEv2GID {
		t.Fatalf("serve failed = %#v", failed)
	}
	results := r.Run(context.Background(), "10.0.0.2", rails, 18515, 3)
	if results[0].Code != proto.RDMABenchNoRoCEv2GID || results[0].Gbps != 0 || results[1].Gbps != 358.74 {
		t.Fatalf("results = %#v", results)
	}
	for line := range rec.lines() {
		if strings.Contains(line, "hns_0") {
			t.Fatalf("tool ran on the GID-less port: %q", line)
		}
	}
}

func TestFailureMessage(t *testing.T) {
	exit := errors.New("exit status 1")
	for _, c := range []struct {
		stdout, stderr string
		err            error
		want           string
	}{
		{refusedStdout, refusedStderr, exit, "Failed to modify QP 92 to RTR; Unable to Connect the HCA's through the link (exit status 1)"},
		{"Couldn't connect to 10.0.0.2:18516\n-----\n", "", exit, "Couldn't connect to 10.0.0.2:18516 (exit status 1)"},
		{"----\n\n", "", exit, "exit status 1"},
		{"", "", nil, "no bandwidth in ib_write_bw output"},
		{"", "a\nb\nc\nd\n", context.DeadlineExceeded, "b; c; d (context deadline exceeded)"},
	} {
		if got := failure([]byte(c.stdout), []byte(c.stderr), c.err); got != c.want {
			t.Errorf("failure(%q, %q) = %q, want %q", c.stdout, c.stderr, got, c.want)
		}
	}
}

func TestMissingToolAndStartFailure(t *testing.T) {
	missing := Runner{Locate: func() (string, error) {
		return "", &missingError{detail: "embedded ib_write_bw cannot start: librdmacm.so.1 missing"}
	}}
	if missing.Available() {
		t.Fatal("available without the tool")
	}
	rails := []Rail{{Device: "mlx5_0", Port: 1}, {Device: "mlx5_1", Port: 1}}
	for name, got := range map[string][]proto.RDMABenchRail{
		"serve": missing.Serve(context.Background(), rails, 18515, 1),
		"run":   missing.Run(context.Background(), "10.0.0.2", rails, 18515, 1),
	} {
		if len(got) != 2 {
			t.Fatalf("%s results = %#v", name, got)
		}
		for _, r := range got {
			if r.Code != proto.RDMABenchToolMissing || !strings.Contains(r.Error, "install perftest") || !strings.Contains(r.Error, "librdmacm") {
				t.Fatalf("%s rail = %#v", name, r)
			}
		}
	}
	failing := Runner{
		Locate: func() (string, error) { return "/x", nil },
		Start: func(context.Context, string, ...string) (func() error, error) {
			return nil, errors.New("permission denied")
		},
	}
	failed := failing.Serve(context.Background(), rails[:1], 1, 1)
	if len(failed) != 1 || failed[0].Code != proto.RDMABenchToolFailed || !strings.Contains(failed[0].Error, "permission denied") {
		t.Fatalf("start failure = %#v", failed)
	}
}
