// kv_disk_quota.go is the belt to the sweeper's braces. The sweeper only
// reacts, once every half minute; an XFS project quota refuses the write that
// would exceed the limit in the first place. It is strictly a bonus - the
// conditions for it are rare enough that the sweeper has to be correct on its
// own - so every unmet precondition is one log line and nothing else.

package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/commandlog"
)

// xfsSuperMagic identifies an XFS filesystem in statfs(2).
const xfsSuperMagic = 0x58465342

// kvDiskQuotaTimeout bounds the xfs_quota call. Quota tooling can block on a
// wedged filesystem, and a stuck instance start is worse than no quota.
const kvDiskQuotaTimeout = 10 * time.Second

// The four facts the quota depends on, each replaceable in a test: what
// filesystem the directory is on, what the kernel says is mounted where, where
// the tool is, and what running it does.
var (
	kvDiskFilesystemType = filesystemType
	kvDiskMountTable     = func() (io.ReadCloser, error) { return os.Open("/proc/self/mounts") }
	kvDiskQuotaBinary    = func() (string, error) { return exec.LookPath("xfs_quota") }
	kvDiskRunQuota       = runXFSQuota
)

// applyKVDiskProjectQuota asks XFS to enforce limit on dir as a project quota,
// using the deployment id as the project id. Releasing the project is not
// needed: removing the directory releases its blocks.
func applyKVDiskProjectQuota(log *slog.Logger, dir string, deploymentID uint, limit int64) {
	mountPoint, reason := xfsProjectQuotaMount(dir)
	if reason != "" {
		log.Info("KV disk tier: no XFS project quota; the sweeper enforces the limit", "dir", dir, "reason", reason)
		return
	}
	binary, err := kvDiskQuotaBinary()
	if err != nil {
		log.Info("KV disk tier: no XFS project quota; the sweeper enforces the limit", "dir", dir, "reason", "xfs_quota is not installed")
		return
	}
	project := strconv.FormatUint(uint64(deploymentID), 10)
	args := []string{
		"-x",
		"-c", fmt.Sprintf("project -s -p %s %s", dir, project),
		"-c", fmt.Sprintf("limit -p bhard=%d %s", limit, project),
		mountPoint,
	}
	execution := commandlog.Start(binary + " " + strings.Join(args, " "))
	output, err := kvDiskRunQuota(binary, args)
	execution.Finish(output, err)
	if err != nil {
		log.Info("KV disk tier: no XFS project quota; the sweeper enforces the limit", "dir", dir, "reason", "xfs_quota was refused", "err", err)
		return
	}
	log.Info("KV disk tier: XFS project quota applied", "dir", dir, "project", project, "limit_bytes", limit)
}

func runXFSQuota(binary string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kvDiskQuotaTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	return string(output), err
}

// xfsProjectQuotaMount answers with the mount point holding dir, or the reason
// no project quota can be set on it.
func xfsProjectQuotaMount(dir string) (string, string) {
	fsType, err := kvDiskFilesystemType(dir)
	if err != nil {
		return "", "the directory could not be inspected: " + err.Error()
	}
	if fsType != xfsSuperMagic {
		return "", "the filesystem is not XFS"
	}
	table, err := kvDiskMountTable()
	if err != nil {
		return "", "the mount table could not be read: " + err.Error()
	}
	defer table.Close()
	mountPoint, ok := projectQuotaMountPoint(table, dir)
	if !ok {
		return "", "the filesystem is not mounted with prjquota"
	}
	return mountPoint, ""
}

// projectQuotaMountPoint finds the longest XFS mount point that is a prefix of
// dir and carries a project quota option. The longest match is the mount dir
// actually lives on, which is the one xfs_quota has to be pointed at.
func projectQuotaMountPoint(table io.Reader, dir string) (string, bool) {
	dir = filepath.Clean(dir)
	best, found := "", false
	scanner := bufio.NewScanner(table)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[2] != "xfs" || !hasProjectQuotaOption(fields[3]) {
			continue
		}
		point := filepath.Clean(unescapeMountField(fields[1]))
		if point != dir && !strings.HasPrefix(dir, strings.TrimSuffix(point, "/")+"/") {
			continue
		}
		if !found || len(point) > len(best) {
			best, found = point, true
		}
	}
	return best, found
}

func hasProjectQuotaOption(options string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == "prjquota" || option == "pquota" {
			return true
		}
	}
	return false
}

// unescapeMountField decodes the octal escapes /proc/self/mounts uses for
// spaces, tabs, newlines and backslashes in a path.
func unescapeMountField(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var out strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+3 < len(value) {
			if decoded, err := strconv.ParseUint(value[index+1:index+4], 8, 8); err == nil {
				out.WriteByte(byte(decoded))
				index += 3
				continue
			}
		}
		out.WriteByte(value[index])
	}
	return out.String()
}
