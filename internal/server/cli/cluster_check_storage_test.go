package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

func TestSummarizeClusterStorageUsesNearestRankPercentiles(t *testing.T) {
	var samples []time.Duration
	for ms := 100; ms >= 1; ms-- { // unsorted input: 1..100 ms
		samples = append(samples, time.Duration(ms)*time.Millisecond)
	}
	got, err := summarizeClusterStorage(samples)
	if err != nil {
		t.Fatal(err)
	}
	want := clusterCheckStorage{Count: 100, P50: 50, P90: 90, P99: 99, Max: 100}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
	// One slow sync in eighty is above the 99th rank: it is the maximum only.
	few := make([]time.Duration, 80)
	for i := range few {
		few[i] = 4 * time.Millisecond
	}
	few[17] = 1300 * time.Millisecond
	got, err = summarizeClusterStorage(few)
	if err != nil || got.P99 != 1300 || got.P90 != 4 || got.Max != 1300 {
		t.Fatalf("eighty samples with one outlier = %+v (%v), want p90 4, p99 1300, max 1300", got, err)
	}
	one, err := summarizeClusterStorage([]time.Duration{7 * time.Millisecond})
	if err != nil || one != (clusterCheckStorage{Count: 1, P50: 7, P90: 7, P99: 7, Max: 7}) {
		t.Fatalf("single sample = %+v (%v)", one, err)
	}
	if _, err := summarizeClusterStorage(nil); err == nil {
		t.Fatal("no samples produced a summary")
	}
}

func TestClassifyClusterStorageBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		p50, p99, max float64
		stalls        int
		want          clusterStorageClass
		tier          storagetier.Tier
	}{
		{"fast", 4, 5, 40, 0, clusterStorageWithinAssumption, storagetier.Standard},
		{"at-assumed-p99", 4, 100, 149.9, 0, clusterStorageWithinAssumption, storagetier.Standard},
		{"just-over-assumed-p99", 4, 100.1, 120, 0, clusterStorageSlowerThanAssumed, storagetier.SlowStorage},
		// p99 18.5 ms with one 2.25 s sync in 30 s: the percentile alone
		// called this member fine.
		{"one-long-stall-under-a-fast-p99", 3.9, 18.5, 2254, 1, clusterStorageSlowerThanAssumed, storagetier.SlowStorage},
		{"max-at-the-step-down", 4, 20, 150, 0, clusterStorageSlowerThanAssumed, storagetier.SlowStorage},
		{"a-counted-stall", 4, 20, 149, 1, clusterStorageSlowerThanAssumed, storagetier.SlowStorage},
		{"field-slow-hosts", 4, 1900, 2900, 14, clusterStorageSlowerThanAssumed, storagetier.SlowStorage},
		{"at-usable-max", 4, 3000, 5000, 20, clusterStorageSlowerThanAssumed, storagetier.SlowStorage},
		{"over-usable-max", 4, 3000, 5000.1, 20, clusterStorageTooSlow, storagetier.SlowStorage},
		{"at-usable-p50", 150, 400, 900, 300, clusterStorageSlowerThanAssumed, storagetier.SlowStorage},
		{"over-usable-p50", 150.1, 400, 900, 300, clusterStorageTooSlow, storagetier.SlowStorage},
	} {
		got := classifyClusterStorage(clusterCheckStorage{P50: tc.p50, P99: tc.p99, Max: tc.max, Stalls: tc.stalls})
		if got != tc.want || got.tier() != tc.tier {
			t.Errorf("%s: class %d tier %s, want %d %s", tc.name, got, got.tier(), tc.want, tc.tier)
		}
	}
}

func TestSummarizeClusterStorageCountsStallsAtTheStandardStepDown(t *testing.T) {
	samples := []time.Duration{3 * time.Millisecond, 149 * time.Millisecond, 150 * time.Millisecond, 2254 * time.Millisecond, 4 * time.Millisecond}
	got, err := summarizeClusterStorage(samples)
	if err != nil || got.Stalls != 2 || got.Max != 2254 || got.Count != 5 {
		t.Fatalf("summary %+v, %v; want 2 stalls of at least 150 ms and max 2254 ms", got, err)
	}
	if clusterStorageStall != 150*time.Millisecond {
		t.Fatalf("stall threshold %s, want the standard tier's 150 ms step-down", clusterStorageStall)
	}
}

func probeLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestClusterStorageProbeMeasuresAndRemovesItsFile(t *testing.T) {
	dir := t.TempDir()
	probe := newClusterStorageProbe()
	probe.pace = 2 * time.Millisecond
	var mode os.FileMode
	var name string
	sync := probe.sync
	probe.sync = func(f *os.File) error {
		if info, err := f.Stat(); err == nil {
			mode, name = info.Mode().Perm(), filepath.Base(f.Name())
		}
		return sync(f)
	}
	started := time.Now()
	got, err := probe.run(t.Context(), dir, 120*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 120*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("probe ran for %s, want about 120ms", elapsed)
	}
	if got.Count < 2 || got.Dir != dir || got.P50 <= 0 || got.P50 > got.P90 || got.P90 > got.P99 || got.P99 > got.Max {
		t.Fatalf("measurement = %+v", got)
	}
	if mode != 0o600 || !strings.HasPrefix(name, ".gpuworks-storage-probe-") {
		t.Fatalf("probe file %q mode %o, want a private .gpuworks-storage-probe-* file", name, mode)
	}
	if left := probeLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("probe left %v", left)
	}
}

func TestClusterStorageProbeRemovesItsFileOnFailureAndCancel(t *testing.T) {
	for _, scenario := range []string{"write-no-space", "sync-io-error", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			foreign := filepath.Join(dir, "operator-file")
			if err := os.WriteFile(foreign, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			probe := newClusterStorageProbe()
			probe.pace = time.Millisecond
			calls := 0
			var want error
			switch scenario {
			case "write-no-space":
				want = syscall.ENOSPC
				write := probe.write
				probe.write = func(f *os.File, p []byte, offset int64) (int, error) {
					if calls++; calls == 3 {
						return 0, &os.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC}
					}
					return write(f, p, offset)
				}
			case "sync-io-error":
				want = syscall.EIO
				probe.sync = func(*os.File) error { return syscall.EIO }
			case "cancel":
				want = context.Canceled
				sync := probe.sync
				probe.sync = func(f *os.File) error {
					if calls++; calls == 2 {
						cancel()
					}
					return sync(f)
				}
			}
			_, err := probe.run(ctx, dir, time.Minute)
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			if left := probeLeftovers(t, dir); !reflect.DeepEqual(left, []string{"operator-file"}) {
				t.Fatalf("directory holds %v, want only the file that was there before", left)
			}
			if raw, err := os.ReadFile(foreign); err != nil || string(raw) != "keep" {
				t.Fatalf("existing file changed: %q %v", raw, err)
			}
		})
	}
}

func TestClusterStorageProbeReportsAnUnusableDirectoryAndDuration(t *testing.T) {
	probe := newClusterStorageProbe()
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := probe.run(t.Context(), missing, time.Second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory error = %v", err)
	}
	if os.Geteuid() != 0 {
		readOnly := t.TempDir()
		if err := os.Chmod(readOnly, 0o500); err != nil {
			t.Fatal(err)
		}
		if _, err := probe.run(t.Context(), readOnly, time.Second); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("unwritable directory error = %v, want permission denied", err)
		}
	}
	for _, duration := range []time.Duration{0, -time.Second, clusterCheckStorageLimit + time.Millisecond} {
		dir := t.TempDir()
		if _, err := probe.run(t.Context(), dir, duration); err == nil || len(probeLeftovers(t, dir)) != 0 {
			t.Fatalf("duration %s: err=%v leftovers=%v", duration, err, probeLeftovers(t, dir))
		}
	}
}

func TestClusterStorageDirectoryIsTheNearestExistingAncestor(t *testing.T) {
	root := t.TempDir()
	if got, err := clusterStorageDirectory(filepath.Join(root, "a", "b", "install")); err != nil || got != root {
		t.Fatalf("directory = %q %v, want %q", got, err, root)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clusterStorageDirectory(filepath.Join(file, "install")); err == nil {
		t.Fatal("a file was accepted as the data filesystem")
	}
	if _, err := clusterStorageDirectory("relative/install"); err == nil {
		t.Fatal("a relative installation directory was accepted")
	}
}

func TestClusterStorageMountPicksTheLongestContainingMountPoint(t *testing.T) {
	table := filepath.Join(t.TempDir(), "mountinfo")
	raw := "25 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw\n" +
		"40 25 0:35 / /data rw - ceph 10.0.0.1:6789:/ rw,name=admin\n" +
		"41 25 252:16 / /data/fast\\040disk rw master:2 - xfs /dev/vdb rw\n" +
		"malformed line\n"
	if err := os.WriteFile(table, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string][2]string{
		"/root":                  {"ext4", "/dev/vda1"},
		"/data/gpuworks":         {"ceph", "10.0.0.1:6789:/"},
		"/database":              {"ext4", "/dev/vda1"},
		"/data/fast disk/server": {"xfs", "/dev/vdb"},
	} {
		if fs, device := clusterStorageMount(table, dir); fs != want[0] || device != want[1] {
			t.Errorf("%s: %s %s, want %v", dir, fs, device, want)
		}
	}
	if fs, device := clusterStorageMount(filepath.Join(t.TempDir(), "none"), "/root"); fs != "" || device != "" {
		t.Fatalf("missing table gave %q %q", fs, device)
	}
}

func TestClusterCheckStorageWireFormat(t *testing.T) {
	request, err := json.Marshal(clusterCheckRequest{Action: "storage", StorageProbeMS: 30000})
	if err != nil || string(request) != `{"action":"storage","storage_probe_ms":30000}` {
		t.Fatalf("request = %s (%v)", request, err)
	}
	response, err := json.Marshal(clusterCheckResponse{Storage: &clusterCheckStorage{Dir: "/root", Filesystem: "ext4", Device: "/dev/vda1", Count: 941, P50: 4.2, P90: 37.9, P99: 1867.8, Max: 2100, Stalls: 31}})
	want := `{"storage":{"dir":"/root","filesystem":"ext4","device":"/dev/vda1","count":941,"p50_ms":4.2,"p90_ms":37.9,"p99_ms":1867.8,"max_ms":2100,"stalls":31}}`
	if err != nil || string(response) != want {
		t.Fatalf("response = %s (%v)\nwant %s", response, err, want)
	}
	var decoded clusterCheckResponse
	decoder := json.NewDecoder(strings.NewReader(want))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil || decoded.Storage == nil || decoded.Storage.P99 != 1867.8 || decoded.Storage.Count != 941 {
		t.Fatalf("decoded = %+v (%v)", decoded.Storage, err)
	}
	if raw, _ := json.Marshal(clusterCheckResponse{}); string(raw) != `{}` {
		t.Fatalf("a reply without a measurement = %s, want {}", raw)
	}
	if raw, _ := json.Marshal(clusterCheckRequest{Action: "stop"}); string(raw) != `{"action":"stop"}` {
		t.Fatalf("a request without a probe = %s", raw)
	}
}

func TestClusterCheckNodeStorageRefusesBadRequestsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	for _, ms := range []int{0, -1, int(clusterCheckStorageLimit/time.Millisecond) + 1} {
		if _, err := clusterCheckNodeStorage(t.Context(), filepath.Join(root, "install"), ms); err == nil {
			t.Fatalf("%d ms was accepted", ms)
		}
	}
	if _, err := clusterCheckNodeStorage(t.Context(), "relative", 100); err == nil {
		t.Fatal("relative installation directory was accepted")
	}
	got, err := clusterCheckNodeStorage(t.Context(), filepath.Join(root, "missing", "install"), 60)
	if err != nil || got.Dir != root || got.Count < 1 {
		t.Fatalf("measurement = %+v (%v), want one taken in %s", got, err, root)
	}
	if left := probeLeftovers(t, root); len(left) != 0 {
		t.Fatalf("node probe left %v", left)
	}
}

// storageClusterCheckSession answers the storage action per host on top of the
// faithful prepare/probe behaviour of the fake session.
type storageClusterCheckSession struct {
	*fakeClusterCheckSession
	storage func(clusterCheckRequest) (clusterCheckResponse, error)
}

func (s *storageClusterCheckSession) Exchange(req clusterCheckRequest) (clusterCheckResponse, error) {
	if req.Action == "storage" {
		s.requests = append(s.requests, req)
		return s.storage(req)
	}
	return s.fakeClusterCheckSession.Exchange(req)
}

func runStorageClusterCheck(t *testing.T, probe time.Duration, answers map[string]func(clusterCheckRequest) (clusterCheckResponse, error)) (string, error, map[string]*storageClusterCheckSession) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ports := checkPorts(t, "127.0.0.1")
	o := clusterInstallOptions{Hosts: []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, Binary: executable, HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Dir: "/tmp/new", StorageProbe: probe}
	sessions := map[string]*storageClusterCheckSession{}
	var output bytes.Buffer
	start := func(_ context.Context, node clusterInstallNode, _ []byte) (clusterCheckSession, error) {
		s := &storageClusterCheckSession{fakeClusterCheckSession: &fakeClusterCheckSession{host: node.Address, port: o.HTTP}, storage: answers[node.Address]}
		sessions[node.Address] = s
		return s, nil
	}
	err = clusterCheckWithStarter(t.Context(), o, &output, start)
	return output.String(), err, sessions
}

func storageAnswer(s clusterCheckStorage) func(clusterCheckRequest) (clusterCheckResponse, error) {
	return func(clusterCheckRequest) (clusterCheckResponse, error) { return clusterCheckResponse{Storage: &s}, nil }
}

func TestClusterCheckReportsTheSlowestNodesStorageClass(t *testing.T) {
	fast := clusterCheckStorage{Dir: "/root", Filesystem: "ext4", Device: "/dev/vda1", Count: 900, P50: 4.2, P90: 5.3, P99: 9.5, Max: 40}
	slow := clusterCheckStorage{Dir: "/root", Filesystem: "ext4", Device: "/dev/vda1", Count: 700, P50: 4.3, P90: 81.3, P99: 1867.8, Max: 2100.4, Stalls: 31}
	dead := clusterCheckStorage{Dir: "/data", Count: 12, P50: 2400, P90: 6000, P99: 7000, Max: 7000, Stalls: 12}
	for _, tc := range []struct {
		name    string
		answers map[string]func(clusterCheckRequest) (clusterCheckResponse, error)
		want    []string
	}{
		{"within", map[string]func(clusterCheckRequest) (clusterCheckResponse, error){"127.0.0.1": storageAnswer(fast), "127.0.0.2": storageAnswer(fast), "127.0.0.3": storageAnswer(fast)},
			[]string{"存储同步写延迟（每个节点 30s，仅供参考，不影响检查结果）", "127.0.0.2 (127.0.0.2)：p50 4.2 ms  p90 5.3 ms  p99 9.5 ms  最大 40.0 ms  ≥150 ms 的停顿 0 次  （900 次，/root，ext4 /dev/vda1）", "存储档位：建议 standard。最慢节点 127.0.0.1 的 p99 为 9.5 ms（不超过 100 ms），没有达到 150 ms 的停顿。"}},
		{"slowest-wins", map[string]func(clusterCheckRequest) (clusterCheckResponse, error){"127.0.0.1": storageAnswer(fast), "127.0.0.2": storageAnswer(slow), "127.0.0.3": storageAnswer(fast)},
			[]string{"127.0.0.2 (127.0.0.2)：p50 4.3 ms  p90 81.3 ms  p99 1867.8 ms  最大 2100.4 ms  ≥150 ms 的停顿 31 次  （700 次，/root，ext4 /dev/vda1）", "存储档位：建议 slow-storage。最慢节点 127.0.0.2 的 p99 为 1867.8 ms、最大 2100.4 ms、≥150 ms 的停顿 31 次；在 standard 档位下空闲时会自行切换数据库主节点。首次启动也较慢。"}},
		{"too-slow", map[string]func(clusterCheckRequest) (clusterCheckResponse, error){"127.0.0.1": storageAnswer(slow), "127.0.0.2": storageAnswer(fast), "127.0.0.3": storageAnswer(dead)},
			[]string{"127.0.0.3 (127.0.0.3)：p50 2400.0 ms  p90 6000.0 ms  p99 7000.0 ms  最大 7000.0 ms  ≥150 ms 的停顿 12 次  （12 次，/data）", "存储档位：过慢，不建议部署集群。最慢节点 127.0.0.3 的 p50 为 2400.0 ms、最大 7000.0 ms，上限为 p50 150 ms、最大 5000 ms。"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err, sessions := runStorageClusterCheck(t, 30*time.Second, tc.answers)
			if err != nil {
				t.Fatalf("a storage class failed the check: %v\n%s", err, output)
			}
			for _, want := range tc.want {
				if !strings.Contains(output, want) {
					t.Fatalf("output lacks %q:\n%s", want, output)
				}
			}
			if !strings.Contains(output, "预检查通过：3/3 节点") || strings.Index(output, "存储同步写延迟") > strings.Index(output, "临时资源清理") || strings.Index(output, "存储同步写延迟") < strings.Index(output, "节点间端口互通") {
				t.Fatalf("storage section is not between the network checks and the cleanup:\n%s", output)
			}
			for host, s := range sessions {
				last := s.requests[len(s.requests)-1]
				if len(s.requests) != 8 || last.Action != "storage" || last.StorageProbeMS != 30000 || !s.closed {
					t.Fatalf("%s: %d requests, last %+v, closed %v", host, len(s.requests), last, s.closed)
				}
			}
		})
	}
}

func TestClusterCheckStorageFailuresAreAdvisory(t *testing.T) {
	fast := clusterCheckStorage{Dir: "/root", Count: 900, P50: 4, P90: 5, P99: 9, Max: 40}
	refused := func(clusterCheckRequest) (clusterCheckResponse, error) {
		return clusterCheckResponse{Error: "storage probe cannot create a file in /opt: permission denied"}, nil
	}
	broken := func(clusterCheckRequest) (clusterCheckResponse, error) {
		return clusterCheckResponse{}, errors.New("read check helper reply: EOF")
	}
	old := func(clusterCheckRequest) (clusterCheckResponse, error) {
		return clusterCheckResponse{Error: "unknown check command"}, nil
	}
	empty := func(clusterCheckRequest) (clusterCheckResponse, error) { return clusterCheckResponse{}, nil }
	output, err, _ := runStorageClusterCheck(t, time.Second, map[string]func(clusterCheckRequest) (clusterCheckResponse, error){"127.0.0.1": storageAnswer(fast), "127.0.0.2": refused, "127.0.0.3": broken})
	if err != nil {
		t.Fatalf("an unmeasured node failed the check: %v", err)
	}
	for _, want := range []string{"[–] 127.0.0.2 (127.0.0.2)：未测量：storage probe cannot create a file in /opt: permission denied", "[–] 127.0.0.3 (127.0.0.3)：未测量：read check helper reply: EOF", "存储档位：建议 standard（仅 1/3 节点已测量）。", "预检查通过：3/3 节点"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	output, err, _ = runStorageClusterCheck(t, time.Second, map[string]func(clusterCheckRequest) (clusterCheckResponse, error){"127.0.0.1": old, "127.0.0.2": empty, "127.0.0.3": old})
	if err != nil || !strings.Contains(output, "存储档位：未测量。") || !strings.Contains(output, "未测量：check helper returned no storage measurement") || !strings.Contains(output, "预检查通过：3/3 节点") {
		t.Fatalf("no measurement at all: %v\n%s", err, output)
	}
}

func TestClusterCheckSkipsStorageWhenDisabled(t *testing.T) {
	unexpected := func(clusterCheckRequest) (clusterCheckResponse, error) {
		return clusterCheckResponse{}, errors.New("storage must not be measured")
	}
	answers := map[string]func(clusterCheckRequest) (clusterCheckResponse, error){"127.0.0.1": unexpected, "127.0.0.2": unexpected, "127.0.0.3": unexpected}
	output, err, sessions := runStorageClusterCheck(t, 0, answers)
	if err != nil || strings.Contains(output, "存储") {
		t.Fatalf("disabled probe: %v\n%s", err, output)
	}
	for host, s := range sessions {
		if len(s.requests) != 7 {
			t.Fatalf("%s received %d requests with the probe disabled", host, len(s.requests))
		}
	}
}

func TestClusterCheckStorageProbeFlag(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "gpuworks")
	if err := os.WriteFile(binary, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	base := []string{"--hosts", "10.0.0.1,10.0.0.2,10.0.0.3", "--ssh-user", "root", "--binary", binary}
	parse := func(check bool, extra ...string) (clusterInstallOptions, error) {
		return parseClusterOptions(append(append([]string(nil), base...), extra...), &bytes.Buffer{}, check)
	}
	if o, err := parse(true); err != nil || o.StorageProbe != 30*time.Second {
		t.Fatalf("default = %s (%v), want 30s", o.StorageProbe, err)
	}
	if o, err := parse(true, "--storage-probe", "0"); err != nil || o.StorageProbe != 0 {
		t.Fatalf("0 = %s (%v), want the probe disabled", o.StorageProbe, err)
	}
	if o, err := parse(true, "--storage-probe", "5m"); err != nil || o.StorageProbe != 5*time.Minute {
		t.Fatalf("5m = %s (%v)", o.StorageProbe, err)
	}
	for _, bad := range []string{"5m1s", "-1s"} {
		if _, err := parse(true, "--storage-probe", bad); err == nil || !strings.Contains(err.Error(), "--storage-probe must be between 0 and 5m0s") {
			t.Fatalf("%s: error %v", bad, err)
		}
	}
	// Installation measures too: it chooses the storage tier from the result.
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("private-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if o, err := parse(false, "--password-file", password); err != nil || o.StorageProbe != 30*time.Second {
		t.Fatalf("install default = %s (%v), want 30s", o.StorageProbe, err)
	}
	if o, err := parse(false, "--password-file", password, "--storage-probe", "0"); err != nil || o.StorageProbe != 0 {
		t.Fatalf("install 0 = %s (%v), want the probe disabled", o.StorageProbe, err)
	}
	if _, err := parse(false, "--password-file", password, "--storage-probe", "6m"); err == nil || !strings.Contains(err.Error(), "--storage-probe must be between 0 and 5m0s") {
		t.Fatalf("install 6m: error %v", err)
	}
}

func TestClusterCheckSkipsStorageAfterAFailedNetworkCheck(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ports := checkPorts(t, "127.0.0.1")
	o := clusterInstallOptions{Hosts: []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, Binary: executable, HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Dir: "/tmp/new", StorageProbe: time.Second}
	var sessions []*storageClusterCheckSession
	var output bytes.Buffer
	start := func(_ context.Context, node clusterInstallNode, _ []byte) (clusterCheckSession, error) {
		s := &storageClusterCheckSession{fakeClusterCheckSession: &fakeClusterCheckSession{host: node.Address, port: o.HTTP, fail: "127.0.0.3"}, storage: func(clusterCheckRequest) (clusterCheckResponse, error) {
			return clusterCheckResponse{}, errors.New("storage must not be measured")
		}}
		sessions = append(sessions, s)
		return s, nil
	}
	if err := clusterCheckWithStarter(t.Context(), o, &output, start); err == nil || strings.Contains(output.String(), "存储") {
		t.Fatalf("err=%v\n%s", err, output.String())
	}
	for _, s := range sessions {
		for _, req := range s.requests {
			if req.Action == "storage" {
				t.Fatal("storage was measured after a failed network check")
			}
		}
	}
}

// The helper answers the storage action only after prepare, on the filesystem
// of the prepared installation directory, and keeps serving afterwards.
func TestClusterCheckNodeAnswersTheStorageAction(t *testing.T) {
	ports := checkPorts(t, "127.0.0.1")
	token := strings.Repeat("b", 64)
	root := t.TempDir()
	var replies bytes.Buffer
	var requests bytes.Buffer
	encoder := json.NewEncoder(&requests)
	for _, req := range []clusterCheckRequest{
		{Action: "prepare", Dir: filepath.Join(root, "install"), Host: "127.0.0.1", HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Token: token, Targets: map[string]string{"127.0.0.1": token}},
		{Action: "storage", StorageProbeMS: 80},
		{Action: "storage"},
		{Action: "storage", StorageProbeMS: int(clusterCheckStorageLimit/time.Millisecond) + 1},
		{Action: "stop"},
	} {
		if err := encoder.Encode(req); err != nil {
			t.Fatal(err)
		}
	}
	if err := clusterCheckNodeRun(t.Context(), &requests, &replies, func(context.Context, clusterCheckRequest) error { return nil }); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&replies)
	decoder.DisallowUnknownFields()
	var got []clusterCheckResponse
	for decoder.More() {
		var response clusterCheckResponse
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		got = append(got, response)
	}
	if len(got) != 5 {
		t.Fatalf("%d replies, want 5", len(got))
	}
	if s := got[1].Storage; got[1].Error != "" || s == nil || s.Dir != root || s.Count < 1 || s.Max < s.P50 {
		t.Fatalf("storage reply = %+v error %q", got[1].Storage, got[1].Error)
	}
	for _, refused := range got[2:4] {
		if refused.Storage != nil || !strings.Contains(refused.Error, "storage probe duration must be within 5m0s") {
			t.Fatalf("out-of-range duration answered %+v %q", refused.Storage, refused.Error)
		}
	}
	if got[4].Error != "" {
		t.Fatalf("stop after storage: %q", got[4].Error)
	}
	if left := probeLeftovers(t, root); len(left) != 0 {
		t.Fatalf("helper left %v", left)
	}
}
