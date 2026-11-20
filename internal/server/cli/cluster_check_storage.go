package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// clusterCheckStorage is one member's synchronous-write latency on the
// filesystem that will hold its data directory, in milliseconds.
type clusterCheckStorage struct {
	Dir        string  `json:"dir"`
	Filesystem string  `json:"filesystem,omitempty"`
	Device     string  `json:"device,omitempty"`
	Count      int     `json:"count"`
	P50        float64 `json:"p50_ms"`
	P90        float64 `json:"p90_ms"`
	P99        float64 `json:"p99_ms"`
	Max        float64 `json:"max_ms"`
	// Stalls counts the syncs that took at least the standard tier's leader
	// step-down time.
	Stalls int `json:"stalls"`
}

const (
	clusterCheckStorageDefault = 30 * time.Second
	clusterCheckStorageLimit   = 5 * time.Minute
)

// The storage classes say which storage tier a cluster on this storage should
// run. Every replicated commit waits for a synchronous write on a majority,
// and the standard tier's leader steps down after about 150 ms without one.
//
//   - clusterStorageAssumedP99: a member whose 99th percentile sync stays
//     under the step-down time with margin, and that never once stalled for
//     as long as it (Stalls is zero), fits the standard tier. The percentile
//     alone is not enough: a member measured at p99 18 ms had one 2.25 s sync
//     in 30 s, and one such stall is a chance to lose leadership.
//   - clusterStorageUsableMax and clusterStorageUsableP50: a membership grant
//     is abandoned after 5 s, and a median sync above the standard step-down
//     time means no tier's leader keeps up. Beyond either a cluster is not
//     recommended.
//
// Between the two the slow-storage tier applies. Members measured at p50
// 4 ms, p90 20-80 ms, p99 0.5-1.9 s changed leader every few minutes while
// idle on the standard tier and once in 27 minutes on slow-storage.
const (
	clusterStorageAssumedP99 = 100.0
	clusterStorageUsableMax  = 5000.0
	clusterStorageUsableP50  = 150.0
)

// clusterStorageStall is the sync duration counted in Stalls: the standard
// tier's leader step-down time.
var clusterStorageStall = storagetier.Standard.ElectionTimeout()

type clusterStorageClass int

const (
	clusterStorageWithinAssumption clusterStorageClass = iota
	clusterStorageSlowerThanAssumed
	clusterStorageTooSlow
)

func classifyClusterStorage(s clusterCheckStorage) clusterStorageClass {
	stall := float64(clusterStorageStall) / float64(time.Millisecond)
	switch {
	case s.Max > clusterStorageUsableMax || s.P50 > clusterStorageUsableP50:
		return clusterStorageTooSlow
	case s.P99 > clusterStorageAssumedP99 || s.Stalls > 0 || s.Max >= stall:
		return clusterStorageSlowerThanAssumed
	default:
		return clusterStorageWithinAssumption
	}
}

// tier is the storage tier a class calls for. A cluster on storage that is
// too slow is not recommended, and if installed anyway it needs the slow one.
func (c clusterStorageClass) tier() storagetier.Tier {
	if c == clusterStorageWithinAssumption {
		return storagetier.Standard
	}
	return storagetier.SlowStorage
}

// summarizeClusterStorage reports nearest-rank percentiles: the smallest
// sample that at least p percent of the samples do not exceed.
func summarizeClusterStorage(samples []time.Duration) (clusterCheckStorage, error) {
	if len(samples) == 0 {
		return clusterCheckStorage{}, errors.New("storage probe completed no synchronous write")
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := func(percent int) float64 {
		index := (len(sorted)*percent + 99) / 100
		return float64(sorted[index-1]) / float64(time.Millisecond)
	}
	stalls := len(sorted) - sort.Search(len(sorted), func(i int) bool { return sorted[i] >= clusterStorageStall })
	return clusterCheckStorage{Count: len(sorted), P50: rank(50), P90: rank(90), P99: rank(99), Max: rank(100), Stalls: stalls}, nil
}

type clusterStorageResult struct {
	storage *clusterCheckStorage
	err     error
}

// clusterStorageVerdict is what the storage section found: the slowest
// measured member and how many of the members were measured at all.
type clusterStorageVerdict struct {
	slowest     *clusterCheckStorage
	slowestName string
	measured    int
	members     int
}

// class is meaningful only when a member was measured.
func (v clusterStorageVerdict) class() clusterStorageClass { return classifyClusterStorage(*v.slowest) }

// clusterCheckStorageSection measures every member at once, so the whole
// section takes one probe duration, and reports the slowest member's class
// and the storage tier it calls for. Nothing here fails the check: a member
// that could not be measured is shown as not measured.
func clusterCheckStorageSection(ctx context.Context, o clusterInstallOptions, output io.Writer, sessions map[string]clusterCheckSession) clusterStorageVerdict {
	verdict := clusterStorageVerdict{members: len(o.Hosts)}
	if o.StorageProbe <= 0 {
		return verdict
	}
	fmt.Fprintf(output, "\n存储同步写延迟（每个节点 %s，仅供参考，不影响检查结果）\n", o.StorageProbe)
	results := make(map[string]clusterStorageResult, len(o.Hosts))
	var guard sync.Mutex
	var pending sync.WaitGroup
	for _, host := range o.Hosts {
		pending.Add(1)
		go func(host string) {
			defer pending.Done()
			result := clusterStorageResult{}
			response, err := sessions[host].Exchange(clusterCheckRequest{Action: "storage", StorageProbeMS: int(o.StorageProbe / time.Millisecond)})
			switch {
			case err != nil:
				result.err = err
			case response.Error != "":
				result.err = errors.New(response.Error)
			case response.Storage == nil:
				result.err = errors.New("check helper returned no storage measurement")
			default:
				result.storage = response.Storage
			}
			guard.Lock()
			results[host] = result
			guard.Unlock()
		}(host)
	}
	pending.Wait()
	for _, host := range o.Hosts {
		name := o.node(host).Name
		result := results[host]
		if result.err != nil {
			clusterCheckLine(output, "–", fmt.Sprintf("%s (%s)", name, host), "未测量："+result.err.Error())
			continue
		}
		s := result.storage
		verdict.measured++
		where := s.Dir
		if s.Filesystem != "" {
			where += "，" + s.Filesystem
		}
		if s.Device != "" {
			where += " " + s.Device
		}
		fmt.Fprintf(output, "  %s (%s)：p50 %.1f ms  p90 %.1f ms  p99 %.1f ms  最大 %.1f ms  ≥%d ms 的停顿 %d 次  （%d 次，%s）\n", name, host, s.P50, s.P90, s.P99, s.Max, clusterStorageStall.Milliseconds(), s.Stalls, s.Count, where)
		if verdict.slowest == nil || classifyClusterStorage(*s) > verdict.class() || (classifyClusterStorage(*s) == verdict.class() && s.Max > verdict.slowest.Max) {
			verdict.slowest, verdict.slowestName = s, name
		}
	}
	if verdict.slowest == nil {
		fmt.Fprintln(output, "  存储档位：未测量。")
		return verdict
	}
	note := ""
	if verdict.measured != verdict.members {
		note = fmt.Sprintf("（仅 %d/%d 节点已测量）", verdict.measured, verdict.members)
	}
	slowest := verdict.slowest
	switch verdict.class() {
	case clusterStorageWithinAssumption:
		fmt.Fprintf(output, "  存储档位：建议 %s%s。最慢节点 %s 的 p99 为 %.1f ms（不超过 %.0f ms），没有达到 %d ms 的停顿。\n", storagetier.Standard, note, verdict.slowestName, slowest.P99, clusterStorageAssumedP99, clusterStorageStall.Milliseconds())
	case clusterStorageSlowerThanAssumed:
		fmt.Fprintf(output, "  存储档位：建议 %s%s。最慢节点 %s 的 p99 为 %.1f ms、最大 %.1f ms、≥%d ms 的停顿 %d 次；在 %s 档位下空闲时会自行切换数据库主节点。首次启动也较慢。\n", storagetier.SlowStorage, note, verdict.slowestName, slowest.P99, slowest.Max, clusterStorageStall.Milliseconds(), slowest.Stalls, storagetier.Standard)
	default:
		fmt.Fprintf(output, "  存储档位：过慢，不建议部署集群%s。最慢节点 %s 的 p50 为 %.1f ms、最大 %.1f ms，上限为 p50 %.0f ms、最大 %.0f ms。\n", note, verdict.slowestName, slowest.P50, slowest.Max, clusterStorageUsableP50, clusterStorageUsableMax)
	}
	return verdict
}
