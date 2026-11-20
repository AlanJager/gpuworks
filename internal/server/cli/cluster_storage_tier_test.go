package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

func storageTierPassword(t *testing.T) string {
	t.Helper()
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("private-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return password
}

func TestClusterInstallStorageTierFlagAndConfigPrecedence(t *testing.T) {
	password := storageTierPassword(t)
	hosts := []string{"--binary", "/tmp/gpuworks-ha", "--hosts", "192.0.2.1,192.0.2.2,192.0.2.3", "--ssh-user", "operator", "--dir", "/home/operator/cluster", "--password-file", password}
	parse := func(extra ...string) (clusterInstallOptions, error) {
		return parseClusterInstall(append(append([]string{}, hosts...), extra...), io.Discard)
	}
	if o, err := parse(); err != nil || o.StorageTier != "" {
		t.Fatalf("no flag: tier %q err %v, want none so the measurement decides", o.StorageTier, err)
	}
	for name, want := range map[string]storagetier.Tier{"standard": storagetier.Standard, "slow-storage": storagetier.SlowStorage} {
		if o, err := parse("--storage-tier", name); err != nil || o.StorageTier != want {
			t.Fatalf("--storage-tier %s: tier %q err %v", name, o.StorageTier, err)
		}
	}
	for _, bad := range []string{"", "slow", "100ms", "Standard"} {
		if _, err := parse("--storage-tier", bad); err == nil || !strings.Contains(err.Error(), "--storage-tier") {
			t.Fatalf("--storage-tier %q: error %v, want a refusal naming the flag", bad, err)
		}
	}
	if _, err := parseClusterOptions(append(append([]string{}, hosts[:len(hosts)-2]...), "--storage-tier", "standard"), io.Discard, true); err == nil {
		t.Fatal("cluster check accepted --storage-tier; it only reports")
	}

	withTier := strings.Replace(clusterConfigFixture, "version: 1\n", "version: 1\nstorage_tier: slow-storage\n", 1)
	if withTier == clusterConfigFixture {
		t.Fatal("fixture has no version line to extend")
	}
	fromConfig := func(content string, extra ...string) (clusterInstallOptions, error) {
		config := writeClusterConfig(t, content)
		return parseClusterInstall(append([]string{"--binary", "/tmp/gpuworks-ha", "--config", config, "--password-file", password}, extra...), io.Discard)
	}
	if o, err := fromConfig(clusterConfigFixture); err != nil || o.StorageTier != "" {
		t.Fatalf("config without the key: tier %q err %v", o.StorageTier, err)
	}
	if o, err := fromConfig(withTier); err != nil || o.StorageTier != storagetier.SlowStorage {
		t.Fatalf("config key: tier %q err %v", o.StorageTier, err)
	}
	if o, err := fromConfig(withTier, "--storage-tier", "standard"); err != nil || o.StorageTier != storagetier.Standard {
		t.Fatalf("flag over config: tier %q err %v, want the flag", o.StorageTier, err)
	}
	if _, err := fromConfig(strings.Replace(withTier, "slow-storage", "glacial", 1)); err == nil || !strings.Contains(err.Error(), "storage_tier") {
		t.Fatalf("unknown config tier: error %v", err)
	}
}

func TestDecideClusterStorageTier(t *testing.T) {
	measured := func(s clusterCheckStorage, measured int) clusterStorageVerdict {
		return clusterStorageVerdict{slowest: &s, slowestName: "b", measured: measured, members: 3}
	}
	fast := clusterCheckStorage{P50: 4, P90: 5, P99: 9.5, Max: 40}
	stalled := clusterCheckStorage{P50: 3.9, P90: 4.8, P99: 18.5, Max: 2254, Stalls: 1}
	dead := clusterCheckStorage{P50: 2400, P90: 6000, P99: 7000, Max: 7000, Stalls: 12}
	unmeasured := clusterStorageVerdict{members: 3}
	for _, tc := range []struct {
		name    string
		given   storagetier.Tier
		verdict clusterStorageVerdict
		want    storagetier.Tier
		line    string
		refused string
	}{
		{"not given, fast", "", measured(fast, 3), storagetier.Standard, "存储档位：standard（按测量选择：最慢节点 b 的 p99 9.5 ms、最大 40.0 ms、≥150 ms 的停顿 0 次）。", ""},
		{"not given, one long stall", "", measured(stalled, 3), storagetier.SlowStorage, "存储档位：slow-storage（按测量选择：最慢节点 b 的 p99 18.5 ms、最大 2254.0 ms、≥150 ms 的停顿 1 次）。", ""},
		{"not given, partly measured", "", measured(stalled, 2), storagetier.SlowStorage, "（仅 2/3 节点已测量）", ""},
		{"not given, too slow", "", measured(dead, 3), "", "", "storage is too slow to recommend a cluster (slowest member b: p50 2400.0 ms, max 7000.0 ms; limits p50 150 ms, max 5000 ms); pass --storage-tier slow-storage to install anyway"},
		{"not given, unmeasured", "", unmeasured, storagetier.Standard, "存储档位：standard（未测量存储同步写延迟，使用默认档位；慢速存储请指定 --storage-tier slow-storage）。", ""},
		{"standard given, fast", storagetier.Standard, measured(fast, 3), storagetier.Standard, "存储档位：standard（已指定；最慢节点 b 的 p99 9.5 ms、最大 40.0 ms、≥150 ms 的停顿 0 次）。", ""},
		{"standard given, slower measured", storagetier.Standard, measured(stalled, 3), storagetier.Standard, "存储档位：standard（已指定）。警告：测量结果建议 slow-storage：", ""},
		{"slow given, fast measured", storagetier.SlowStorage, measured(fast, 3), storagetier.SlowStorage, "存储档位：slow-storage（已指定；", ""},
		{"slow given, too slow", storagetier.SlowStorage, measured(dead, 3), storagetier.SlowStorage, "存储档位：slow-storage（已指定）。警告：存储过慢，不建议部署集群：", ""},
		{"standard given, too slow", storagetier.Standard, measured(dead, 3), storagetier.Standard, "存储档位：standard（已指定）。警告：存储过慢，不建议部署集群：", ""},
		{"slow given, unmeasured", storagetier.SlowStorage, unmeasured, storagetier.SlowStorage, "存储档位：slow-storage（已指定；未测量存储同步写延迟）。", ""},
	} {
		got, line, err := decideClusterStorageTier(tc.given, tc.verdict)
		if tc.refused != "" {
			if err == nil || err.Error() != tc.refused || got != "" {
				t.Errorf("%s: tier %q error %v, want refusal %q", tc.name, got, err, tc.refused)
			}
			continue
		}
		if err != nil || got != tc.want || !strings.Contains(line, tc.line) {
			t.Errorf("%s: tier %q line %q err %v; want %q with %q", tc.name, got, line, err, tc.want, tc.line)
		}
	}
}

// The installer measures before it writes anything remotely and hands the
// chosen tier to every member action.
func TestClusterCheckMeasureReturnsTheSlowestMembersStorage(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ports := checkPorts(t, "127.0.0.1")
	o := clusterInstallOptions{Hosts: []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, Binary: executable, HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Dir: "/opt/gpuworks/server", NoService: true, StorageProbe: time.Second}
	fast := clusterCheckStorage{Dir: "/root", Count: 900, P50: 4.2, P90: 5.3, P99: 9.5, Max: 40}
	stalled := clusterCheckStorage{Dir: "/root", Count: 883, P50: 3.9, P90: 4.8, P99: 18.5, Max: 2254, Stalls: 1}
	answers := map[string]func(clusterCheckRequest) (clusterCheckResponse, error){"127.0.0.1": storageAnswer(fast), "127.0.0.2": storageAnswer(stalled), "127.0.0.3": storageAnswer(fast)}
	start := func(_ context.Context, node clusterInstallNode, _ []byte) (clusterCheckSession, error) {
		return &storageClusterCheckSession{fakeClusterCheckSession: &fakeClusterCheckSession{host: node.Address, port: o.HTTP}, storage: answers[node.Address]}, nil
	}
	var output bytes.Buffer
	verdict, err := clusterCheckMeasure(t.Context(), o, &output, start)
	if err != nil || verdict.slowest == nil || verdict.slowest.Max != 2254 || verdict.slowestName != "127.0.0.2" || verdict.measured != 3 || verdict.members != 3 {
		t.Fatalf("verdict %+v err %v\n%s", verdict, err, output.String())
	}
	if tier, _, err := decideClusterStorageTier("", verdict); err != nil || tier != storagetier.SlowStorage {
		t.Fatalf("tier %q err %v, want slow-storage for a member that stalled once", tier, err)
	}
	o.StorageProbe = 0
	verdict, err = clusterCheckMeasure(t.Context(), o, io.Discard, start)
	if err != nil || verdict.slowest != nil || verdict.members != 3 {
		t.Fatalf("skipped probe: verdict %+v err %v, want nothing measured", verdict, err)
	}
	if tier, line, err := decideClusterStorageTier("", verdict); err != nil || tier != storagetier.Standard || !strings.Contains(line, "未测量") {
		t.Fatalf("skipped probe: tier %q line %q err %v", tier, line, err)
	}
}

func TestClusterInstallNodeRequestCarriesTheRecordedTier(t *testing.T) {
	standard, err := json.Marshal(clusterInstallNodeRequest{Action: "seed", Dir: "/d", StorageTier: storagetier.Standard.Recorded()})
	if err != nil || strings.Contains(string(standard), "StorageTier") {
		t.Fatalf("standard request = %s (%v), want no tier field", standard, err)
	}
	slow, err := json.Marshal(clusterInstallNodeRequest{Action: "seed", Dir: "/d", StorageTier: storagetier.SlowStorage.Recorded()})
	if err != nil || !strings.Contains(string(slow), `"StorageTier":"slow-storage"`) {
		t.Fatalf("slow request = %s (%v)", slow, err)
	}
	if got := requestStorageTier(clusterInstallNodeRequest{}); got != "standard" {
		t.Fatalf("member command tier for an untiered request = %q, want standard", got)
	}
	if got := requestStorageTier(clusterInstallNodeRequest{StorageTier: "slow-storage"}); got != "slow-storage" {
		t.Fatalf("member command tier = %q", got)
	}
}

func TestParseStorageTierFlag(t *testing.T) {
	if tier, err := parseStorageTierFlag(""); err != nil || tier != "" {
		t.Fatalf("absent flag = %q, %v", tier, err)
	}
	if tier, err := parseStorageTierFlag("slow-storage"); err != nil || tier != storagetier.SlowStorage {
		t.Fatalf("slow-storage = %q, %v", tier, err)
	}
	if _, err := parseStorageTierFlag("fast"); err == nil || err.Error() != `--storage-tier: unknown storage tier "fast": want standard or slow-storage` {
		t.Fatalf("unknown tier error = %v", err)
	}
}
