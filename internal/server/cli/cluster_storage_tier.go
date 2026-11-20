package cli

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// useRecordedStorageTier makes every database node this command starts run
// the storage tier the installation in dataDir records. A command that opens
// the replicated database calls it first; an installation that records no
// tier, or is not a cluster member, runs the standard one.
func useRecordedStorageTier(dataDir string) error {
	tier, err := app.RecordedStorageTier(dataDir)
	if err != nil {
		return fmt.Errorf("storage tier of %s: %w", dataDir, err)
	}
	cluster.UseStorageTier(tier)
	return nil
}

// parseStorageTierFlag reads --storage-tier. Empty means the flag was not
// given, which a caller decides for itself.
func parseStorageTierFlag(name string) (storagetier.Tier, error) {
	if name == "" {
		return "", nil
	}
	tier, err := storagetier.Parse(name)
	if err != nil {
		return "", fmt.Errorf("--storage-tier: %w", err)
	}
	return tier, nil
}

// requestStorageTier names the tier an installation request carries, for a
// member command's --storage-tier.
func requestStorageTier(req clusterInstallNodeRequest) string {
	if req.StorageTier == "" {
		return string(storagetier.Standard)
	}
	return req.StorageTier
}

// decideClusterStorageTier chooses the tier an installation records, from the
// one the operator named (empty: none) and what the storage measurement found.
// It returns the line the installation prints. Storage too slow for any tier
// stops an installation whose operator named none.
func decideClusterStorageTier(given storagetier.Tier, v clusterStorageVerdict) (storagetier.Tier, string, error) {
	if v.slowest == nil {
		if given != "" {
			return given, fmt.Sprintf("存储档位：%s（已指定；未测量存储同步写延迟）。", given), nil
		}
		return storagetier.Standard, fmt.Sprintf("存储档位：%s（未测量存储同步写延迟，使用默认档位；慢速存储请指定 --storage-tier %s）。", storagetier.Standard, storagetier.SlowStorage), nil
	}
	measured := fmt.Sprintf("最慢节点 %s 的 p99 %.1f ms、最大 %.1f ms、≥%d ms 的停顿 %d 次", v.slowestName, v.slowest.P99, v.slowest.Max, clusterStorageStall.Milliseconds(), v.slowest.Stalls)
	if v.measured != v.members {
		measured += fmt.Sprintf("（仅 %d/%d 节点已测量）", v.measured, v.members)
	}
	class, recommended := v.class(), v.class().tier()
	switch {
	case given == "" && class == clusterStorageTooSlow:
		return "", "", fmt.Errorf("storage is too slow to recommend a cluster (slowest member %s: p50 %.1f ms, max %.1f ms; limits p50 %.0f ms, max %.0f ms); pass --storage-tier %s to install anyway", v.slowestName, v.slowest.P50, v.slowest.Max, clusterStorageUsableP50, clusterStorageUsableMax, storagetier.SlowStorage)
	case given == "":
		return recommended, fmt.Sprintf("存储档位：%s（按测量选择：%s）。", recommended, measured), nil
	case class == clusterStorageTooSlow:
		return given, fmt.Sprintf("存储档位：%s（已指定）。警告：存储过慢，不建议部署集群：%s。", given, measured), nil
	case recommended.Slower(given):
		return given, fmt.Sprintf("存储档位：%s（已指定）。警告：测量结果建议 %s：%s；在 %s 档位下空闲时会自行切换数据库主节点。", given, recommended, measured, given), nil
	default:
		return given, fmt.Sprintf("存储档位：%s（已指定；%s）。", given, measured), nil
	}
}
