package testenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestFakeDriverSimulatorHooksAndArchive(t *testing.T) {
	d := NewFakeDriver("fake")
	var seen runtime.WorkloadSpec
	d.AfterRun(func(spec runtime.WorkloadSpec) { seen = spec })
	d.OnRun(func(spec runtime.WorkloadSpec) (string, error, bool) {
		return "hooked", nil, true
	})
	id, err := d.Run(context.Background(), runtime.WorkloadSpec{Name: "hooked-model"})
	if err != nil || id != "hooked" || seen.Name != "hooked-model" {
		t.Fatalf("run hook id=%q err=%v seen=%+v", id, err, seen)
	}
	d.FailNext("PullImage", errors.New("pull failed"))
	if err := d.PullImage(context.Background(), "image", nil); err == nil || err.Error() != "pull failed" {
		t.Fatalf("FailNext pull err=%v", err)
	}
	d.OnStatus(func(string) (runtime.Status, error, bool) {
		return runtime.Status{Exists: true, Health: "healthy"}, nil, true
	})
	status, err := d.Status(context.Background(), "container")
	if err != nil || !status.Exists || status.Health != "healthy" {
		t.Fatalf("status hook=%+v err=%v", status, err)
	}
	d.SetArchiveSize(1024)
	var archive []byte
	if err := d.SaveImage(context.Background(), "image", writerFunc(func(p []byte) (int, error) { archive = append(archive, p...); return len(p), nil })); err != nil {
		t.Fatal(err)
	}
	if len(archive) != 1024 {
		t.Fatalf("archive bytes=%d", len(archive))
	}
	if err := d.LoadImage(context.Background(), "image", io.NopCloser(&readerFunc{data: archive})); err != nil {
		t.Fatal(err)
	}
	present, err := d.ImageExists(context.Background(), "image")
	if err != nil || !present {
		t.Fatalf("loaded image present=%v err=%v", present, err)
	}
}

func TestFakeProberMetricsHooks(t *testing.T) {
	p := NewFakeProber(nil)
	p.OnMetrics(func(context.Context) ([]gpu.Metric, error, bool) {
		return []gpu.Metric{{Index: 2, Values: map[string]float64{"util": 9}}}, nil, true
	})
	metrics, err := p.Metrics(context.Background())
	if err != nil || len(metrics) != 1 || metrics[0].Index != 2 {
		t.Fatalf("metrics=%+v err=%v", metrics, err)
	}
}

func TestFakeDriverImageCacheAndNameAdoption(t *testing.T) {
	d := NewFakeDriver("fake")
	present, err := d.ImageExists(context.Background(), "missing")
	if err != nil || present {
		t.Fatalf("fresh image present=%v err=%v, want absent", present, err)
	}
	_, err = d.Run(context.Background(), runtime.WorkloadSpec{Name: "gpuworks-adopt", Ports: []int{0}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.CloseAll()
	status, err := d.Status(context.Background(), "gpuworks-adopt")
	if err != nil || !status.Exists || !status.Running {
		t.Fatalf("name adoption status=%+v err=%v", status, err)
	}
}

func TestFakeDriverFailNextStopAndRemoveFailBeforeAction(t *testing.T) {
	d := NewFakeDriver("fake")
	defer d.CloseAll()
	id, err := d.Run(context.Background(), runtime.WorkloadSpec{Name: "fail-before-action", Ports: []int{0}})
	if err != nil {
		t.Fatal(err)
	}
	d.FailNext("Stop", errors.New("stop failed"))
	if err := d.Stop(context.Background(), id); err == nil {
		t.Fatal("Stop unexpectedly succeeded")
	}
	if st, err := d.Status(context.Background(), id); err != nil || !st.Running {
		t.Fatalf("failed Stop changed runtime state: %+v err=%v", st, err)
	}
	d.FailNext("Remove", errors.New("remove failed"))
	if err := d.Remove(context.Background(), id); err == nil {
		t.Fatal("Remove unexpectedly succeeded")
	}
	if st, err := d.Status(context.Background(), id); err != nil || !st.Exists {
		t.Fatalf("failed Remove deleted runtime state: %+v err=%v", st, err)
	}
}

func TestFakeDriverListContainersTracksRunAndRemove(t *testing.T) {
	d := NewFakeDriver("fake")
	defer d.CloseAll()
	names, err := d.ListContainers(context.Background())
	if err != nil || len(names) != 0 {
		t.Fatalf("fresh driver containers=%v err=%v, want empty", names, err)
	}
	id, err := d.Run(context.Background(), runtime.WorkloadSpec{Name: "gpuworks-11111111-222", Ports: []int{0}})
	if err != nil {
		t.Fatal(err)
	}
	// Stopping is not removal: the orphan sweeper still sees the container, just
	// like `docker ps -a` does.
	if err := d.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if names, err = d.ListContainers(context.Background()); err != nil || !slices.Equal(names, []string{"gpuworks-11111111-222"}) {
		t.Fatalf("containers after Stop = %v, err=%v", names, err)
	}
	if err := d.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if names, err = d.ListContainers(context.Background()); err != nil || len(names) != 0 {
		t.Fatalf("containers after Remove = %v, err=%v", names, err)
	}

	d.FailNext("ListContainers", errors.New("runtime is unavailable"))
	if _, err := d.ListContainers(context.Background()); err == nil {
		t.Fatal("a failed listing was reported as an empty container set")
	}
}

func TestFakeDriverStopPreservesExistenceButClearsRunning(t *testing.T) {
	d := NewFakeDriver("fake")
	defer d.CloseAll()
	id, err := d.Run(context.Background(), runtime.WorkloadSpec{Name: "stoppable", Ports: []int{0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(context.Background(), "stoppable"); err != nil {
		t.Fatal(err)
	}
	byName, err := d.Status(context.Background(), "stoppable")
	if err != nil || !byName.Exists || byName.Running || byName.Health != "" {
		t.Fatalf("stopped status by name=%+v err=%v, want existing and not running", byName, err)
	}
	byID, err := d.Status(context.Background(), id)
	if err != nil || !byID.Exists || byID.Running {
		t.Fatalf("stopped status by id=%+v err=%v, want existing and not running", byID, err)
	}
	if err := d.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	removed, err := d.Status(context.Background(), id)
	if err != nil || removed.Exists {
		t.Fatalf("removed status=%+v err=%v, want absent", removed, err)
	}
}

func TestFakeDriverExitKeepsTheContainerWithItsCodeUntilRemoved(t *testing.T) {
	d := NewFakeDriver("fake")
	defer d.CloseAll()
	id, err := d.Run(context.Background(), runtime.WorkloadSpec{Name: "crashing", Ports: []int{0}})
	if err != nil {
		t.Fatal(err)
	}
	d.Exit("crashing", 137)
	st, err := d.Status(context.Background(), "crashing")
	if err != nil || st != (runtime.Status{Exists: true, ExitCode: 137}) {
		t.Fatalf("exited status=%+v err=%v", st, err)
	}
	// docker stop on an exited container changes nothing.
	if err := d.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(context.Background(), id); st.ExitCode != 137 {
		t.Fatalf("status after Stop = %+v, want the exit code kept", st)
	}
	if err := d.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if st, err := d.Status(context.Background(), "crashing"); err != nil || st.Exists {
		t.Fatalf("removed status=%+v err=%v", st, err)
	}
	// A replacement under the same name starts with a clean record.
	if _, err := d.Run(context.Background(), runtime.WorkloadSpec{Name: "crashing", Ports: []int{0}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(context.Background(), "crashing"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(context.Background(), "crashing"); st != (runtime.Status{Exists: true}) {
		t.Fatalf("stopped replacement status = %+v", st)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

type readerFunc struct{ data []byte }

func (r *readerFunc) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// The running listing follows the same state Status reads: a started
// container is listed, a stopped, exited or removed one is not.
func TestFakeDriverRunningContainersAgreesWithStatus(t *testing.T) {
	d := NewFakeDriver("fake")
	defer d.CloseAll()
	ctx := context.Background()
	if names, err := d.RunningContainers(ctx); err != nil || len(names) != 0 {
		t.Fatalf("fresh driver running=%v err=%v, want empty", names, err)
	}
	for _, name := range []string{"serving", "stopped", "crashed", "removed"} {
		if _, err := d.Run(ctx, runtime.WorkloadSpec{Name: name, Ports: []int{0}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Stop(ctx, "stopped"); err != nil {
		t.Fatal(err)
	}
	d.Exit("crashed", 137)
	if err := d.Remove(ctx, "removed"); err != nil {
		t.Fatal(err)
	}
	names, err := d.RunningContainers(ctx)
	if err != nil || !slices.Equal(names, []string{"serving"}) {
		t.Fatalf("running=%v err=%v, want only the serving container", names, err)
	}
	for _, name := range []string{"serving", "stopped", "crashed", "removed"} {
		st, err := d.Status(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if listed := slices.Contains(names, name); listed != (st.Exists && st.Running) {
			t.Fatalf("%s: listed=%v but Status=%+v", name, listed, st)
		}
	}

	// A status hook that reports the container dead takes it off the listing
	// too, so a test staging an exit through the hook is not hidden from the
	// liveness monitor.
	d.OnStatus(func(id string) (runtime.Status, error, bool) {
		return runtime.Status{Exists: true, ExitCode: 1}, nil, id == "serving"
	})
	if names, err := d.RunningContainers(ctx); err != nil || len(names) != 0 {
		t.Fatalf("running with the hook reporting an exit = %v, err=%v", names, err)
	}
	d.ResetHooks()

	d.FailNext("RunningContainers", errors.New("runtime is unavailable"))
	if names, err := d.RunningContainers(ctx); err == nil || names != nil {
		t.Fatalf("a failed listing returned %v, err=%v", names, err)
	}
}

// With shared engine ports each fake is its own machine: another node's
// instance on a port number is no conflict, its own running one is, and the
// port frees once that instance stops.
func TestBindHostPortSharedIsPerNode(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	a, b := NewFakeDriver("fake"), NewFakeDriver("fake")
	a.ShareEnginePorts()
	b.ShareEnginePorts()
	id, err := a.Run(context.Background(), runtime.WorkloadSpec{Name: "a-inst", Ports: []int{port}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Remove(context.Background(), id)
	if c, err := b.BindHostPort(port); err != nil {
		t.Fatalf("other node's port: err = %v", err)
	} else {
		_ = c.Close()
	}
	if _, err := a.BindHostPort(port); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("own running instance's port: err = %v, want EADDRINUSE", err)
	}
	if err := a.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BindHostPort(port); err != nil {
		t.Fatalf("after stop: err = %v", err)
	}
}

// Without shared ports two nodes given the same port check it at once (a
// multi-node group's leader and follower start together). Their probe binds
// must not see each other: neither machine uses the port. A real listener on
// the host still refuses it.
func TestBindHostPortChecksOfTwoNodesDoNotCollide(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	a, b := NewFakeDriver("fake"), NewFakeDriver("fake")
	errs := make(chan error, 400)
	var wg sync.WaitGroup
	for _, d := range []*FakeDriver{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				c, err := d.BindHostPort(port)
				if err != nil {
					errs <- err
					continue
				}
				_ = c.Close()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent checks of one port: %v", err)
	}

	busy, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if _, err := a.BindHostPort(port); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("port with a real listener: err = %v, want EADDRINUSE", err)
	}
}

// A shared port's engine lists the model of every instance on it, so each
// instance's own-model check passes as on a host of its own; a stopped
// instance's model leaves the list.
func TestSharedPortListsEveryInstanceModel(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	a, b := NewFakeDriver("fake"), NewFakeDriver("fake")
	a.ShareEnginePorts()
	b.ShareEnginePorts()
	ctx := context.Background()
	idA, err := a.Run(ctx, runtime.WorkloadSpec{Name: "a", Ports: []int{port}, Cmd: []string{"--served-model-name", "model-a"}})
	if err != nil {
		t.Fatal(err)
	}
	idB, err := b.Run(ctx, runtime.WorkloadSpec{Name: "b", Ports: []int{port}, Cmd: []string{"--served-model-name", "model-b"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Remove(ctx, idA)
	models := func() []string {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range list.Data {
			out = append(out, m.ID)
		}
		return out
	}
	if got := models(); !slices.Equal(got, []string{"model-a", "model-b"}) {
		t.Fatalf("models = %v", got)
	}
	if err := b.Stop(ctx, idB); err != nil {
		t.Fatal(err)
	}
	if err := b.Remove(ctx, idB); err != nil {
		t.Fatal(err)
	}
	if got := models(); !slices.Equal(got, []string{"model-a"}) {
		t.Fatalf("after b left: models = %v", got)
	}
}
