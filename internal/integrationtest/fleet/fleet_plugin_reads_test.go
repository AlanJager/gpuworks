package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// TestFleetPluginPollingNodeInventory is the cost of a plugin that pages
// through the node inventory as fast as the host API lets it: at fleet size
// (1000 nodes with cards, on a file database), a plugin speaking real gwx/1
// to the real plugin host reads /host/v1/nodes in a loop, backing off only
// when the page bucket refuses it, and the case holds the server's CPU and
// memory to a budget. The plugin runs in this process, so the budget also
// covers the plugin's side; the server's share is smaller.
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 30m ./internal/integrationtest/fleet/ -run TestFleetPluginPollingNodeInventory
//
// sized by GPUWORKS_FLEET_NODES and GPUWORKS_FLEET_PLUGIN_POLL_SECONDS.
func TestFleetPluginPollingNodeInventory(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("fleet load case; set GPUWORKS_TEST_PERF=1 on an idle host to run (full command in the test's doc comment)")
	}
	requireFileDescriptors(t, 256)
	nodeCount := EnvInt("GPUWORKS_FLEET_NODES", 1000)
	pollFor := time.Duration(EnvInt("GPUWORKS_FLEET_PLUGIN_POLL_SECONDS", 30)) * time.Second
	const (
		cardsPerNode = 8
		// cpuBudget is the CPU this process (host and plugin) may spend
		// per second of polling: the page bucket allows two pages of 500
		// nodes a second. Measured 2026-11 at 1000 nodes with 8 cards:
		// 0.24 core in all, about half of it the host side (the store's
		// two reads and the canonical encoding of the page), the rest the
		// SDK's strict decoding and the garbage collector.
		cpuBudget = 0.5
		rssBudget = 128.0 // MiB of growth while polling
	)

	st, err := store.Open(filepath.Join(t.TempDir(), "fleet.sqlite"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedInventory(t, st, nodeCount, cardsPerNode)

	var pages, refused, sweeps atomic.Int64
	polling := make(chan struct{})
	var pollingOnce sync.Once
	spawner := &inProcessSpawner{cfg: pluginsdk.Config{
		ID: "acme_inventory", Version: "1.0.0", ManifestSHA256: fleetPluginDigest,
		Requires: map[string]extapi.Range{extapi.CapHostNodes: {Min: 1, Max: 1}},
		Start: func(ctx context.Context, p *pluginsdk.Plugin) error {
			var q extapi.NodesQuery
			seen := 0
			for ctx.Err() == nil {
				page, err := p.Nodes(ctx, q)
				var refusal *pluginsdk.Error
				switch {
				case errors.As(err, &refusal) && refusal.Wire.Reason == extapi.ReasonBusy:
					refused.Add(1)
					time.Sleep(time.Duration(refusal.Wire.RetryAfterMS) * time.Millisecond)
					continue
				case err != nil:
					if ctx.Err() == nil {
						t.Errorf("page: %v", err)
					}
					return nil
				}
				pages.Add(1)
				pollingOnce.Do(func() { close(polling) })
				seen += len(page.Nodes)
				q.Cursor = page.Next
				if page.Next == "" {
					if seen != nodeCount {
						t.Errorf("a sweep saw %d nodes, want %d", seen, nodeCount)
					}
					sweeps.Add(1)
					seen = 0
				}
			}
			return nil
		},
	}}
	host, err := pluginhost.New(pluginhost.Config{
		Spawner: spawner, Reads: app.PluginReads(st), Log: slog.New(slog.DiscardHandler),
		Core: func() extapi.CoreInfo {
			return extapi.CoreInfo{Version: "v0.0.0-fleet", InstallID: "fleet_install", OS: "linux", Arch: "amd64",
				HARole: extapi.HARoleSingle, Offer: extpkg.Offer()}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = host.Close(ctx)
	})
	dir := t.TempDir()
	if err := host.Start(pluginhost.Spec{ID: "acme_inventory", Version: "1.0.0", ManifestSHA256: fleetPluginDigest,
		Exe: "/pkg/bin/acme_inventory", PackageDir: "/pkg", DataDir: dir, TmpDir: dir, Lifecycle: extapi.LifecycleResident,
		Grant:    extapi.PluginGrant{ID: "acme_inventory", Version: "1.0.0", PermissionsGranted: []string{"read:node_inventory"}},
		Approved: map[string]int{extapi.CapHostNodes: 1}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-polling:
	case <-time.After(time.Minute):
		st, _ := host.Status("acme_inventory")
		t.Fatalf("the plugin read no page: %+v", st)
	}

	cpu0, rss0, pages0, start := processCPUSeconds(), processRSSMiB(), pages.Load(), time.Now()
	time.Sleep(pollFor)
	wall := time.Since(start).Seconds()
	cpu, rss, read := processCPUSeconds()-cpu0, processRSSMiB()-rss0, pages.Load()-pages0
	t.Logf("%d nodes, %.0fs: %d pages (%.2f/s), %d sweeps, %d refusals; CPU %.2fs (%.3f core), RSS %+.1f MiB",
		nodeCount, wall, read, float64(read)/wall, sweeps.Load(), refused.Load(), cpu, cpu/wall, rss)
	if maxPages := int64(wall*2) + 4 + 1; read > maxPages {
		t.Errorf("read %d pages in %.0fs; the page bucket allows %d", read, wall, maxPages)
	}
	if refused.Load() == 0 {
		t.Error("the plugin was never refused a page; it did not poll as fast as allowed")
	}
	if cpu/wall > cpuBudget {
		t.Errorf("CPU %.3f core while a plugin polls the inventory, budget %.2f", cpu/wall, cpuBudget)
	}
	if rss > rssBudget {
		t.Errorf("RSS grew %.1f MiB while a plugin polls the inventory, budget %.0f", rss, rssBudget)
	}
}

const fleetPluginDigest = extapi.Digest("sha256:69a8fc5cf9fa0174e3dfe74b1c9c1f5f4f64710afa362419883f19c6b42ee422")

// seedInventory enrolls n nodes, each reporting a machine UUID and cards
// cards with hardware serials, as their agents would.
func seedInventory(t *testing.T, st *store.Store, n, cards int) {
	t.Helper()
	ok := true
	for i := range n {
		node, _, err := st.CreateNodePending(fmt.Sprintf("fleet-%04d", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeHostSystemUUID(node.UUID, fmt.Sprintf("4c4c4544-0042-3510-8051-%012x", i)); err != nil {
			t.Fatal(err)
		}
		gpus := make([]proto.GPUInfo, cards)
		for j := range gpus {
			gpus[j] = proto.GPUInfo{Index: j, UUID: fmt.Sprintf("GPU-%08x-0000-1111-2222-%012x", i, j),
				PCIeBDF: fmt.Sprintf("0000:%02x:00.0", j+1), Vendor: "nvidia", Name: "NVIDIA H800", MemoryTotalBytes: 80 << 30, Healthy: true}
		}
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUProbeOK: &ok, GPUs: gpus, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
			t.Fatal(err)
		}
	}
}

// inProcessSpawner runs the plugin as pluginsdk.Serve in a goroutine over
// loopback connections, so the plugin host speaks real gwx/1 to the real
// SDK without a binary.
type inProcessSpawner struct{ cfg pluginsdk.Config }

func (s *inProcessSpawner) Spawn(_ context.Context, _ pluginhost.SpawnRequest) (pluginhost.Process, error) {
	serveCore, servePlugin, err := loopbackPair()
	if err != nil {
		return nil, err
	}
	hostCore, hostPlugin, err := loopbackPair()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &inProcess{serve: serveCore, host: hostCore, done: make(chan struct{}), cancel: cancel}
	cfg := s.cfg
	cfg.Logger = slog.New(slog.DiscardHandler)
	go func() {
		defer close(p.done)
		_ = pluginsdk.Serve(ctx, cfg, servePlugin, hostPlugin)
		_ = servePlugin.Close()
		_ = hostPlugin.Close()
	}()
	return p, nil
}

type inProcess struct {
	serve, host net.Conn
	done        chan struct{}
	cancel      context.CancelFunc
}

func (p *inProcess) PID() int                    { return os.Getpid() }
func (p *inProcess) Serve() net.Conn             { return p.serve }
func (p *inProcess) Host() net.Conn              { return p.host }
func (p *inProcess) Stderr() io.Reader           { return eofReader{} }
func (p *inProcess) Done() <-chan struct{}       { return p.done }
func (p *inProcess) Exit() pluginhost.ExitStatus { return pluginhost.ExitStatus{} }
func (p *inProcess) Signal(os.Signal) error      { p.cancel(); return nil }

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

func loopbackPair() (net.Conn, net.Conn, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()
	dialed, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		return nil, nil, err
	}
	other := <-accepted
	if other == nil {
		dialed.Close()
		return nil, nil, errors.New("loopback accept failed")
	}
	return other, dialed, nil
}
