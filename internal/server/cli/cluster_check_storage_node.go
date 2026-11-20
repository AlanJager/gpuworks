package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	clusterStorageBlock = 4 << 10
	// The scratch file never grows past this, so the probe needs 4 MiB.
	clusterStorageFileLimit = 4 << 20
	// At most 40 synchronous writes a second: light enough to run beside a
	// working database on the same disk.
	clusterStoragePace = 25 * time.Millisecond
)

// clusterStorageProbe measures synchronous 4 KiB writes. The fields are the
// operations a test replaces to make a write or a sync fail.
type clusterStorageProbe struct {
	write func(*os.File, []byte, int64) (int, error)
	sync  func(*os.File) error
	pace  time.Duration
}

func newClusterStorageProbe() clusterStorageProbe {
	return clusterStorageProbe{
		write: func(f *os.File, p []byte, offset int64) (int, error) { return f.WriteAt(p, offset) },
		sync:  clusterStorageSync,
		pace:  clusterStoragePace,
	}
}

// clusterStorageDirectory is the nearest existing ancestor of the installation
// directory: the filesystem the member's data will be written to.
func clusterStorageDirectory(installDir string) (string, error) {
	if !filepath.IsAbs(installDir) {
		return "", errors.New("installation directory must be absolute")
	}
	dir := filepath.Dir(filepath.Clean(installDir))
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", dir)
			}
			return dir, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		return "", err
	}
}

// run writes only to a file it created exclusively in dir and removes that
// file before returning, also when a write fails or ctx ends.
func (p clusterStorageProbe) run(ctx context.Context, dir string, duration time.Duration) (result clusterCheckStorage, resultErr error) {
	if duration <= 0 || duration > clusterCheckStorageLimit {
		return result, fmt.Errorf("storage probe duration must be within %s", clusterCheckStorageLimit)
	}
	file, err := os.CreateTemp(dir, ".gpuworks-storage-probe-*")
	if err != nil {
		return result, fmt.Errorf("storage probe cannot create a file in %s: %w", dir, err)
	}
	name := file.Name()
	defer func() {
		closeErr := file.Close()
		if removeErr := os.Remove(name); removeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove storage probe file: %w", removeErr))
		} else if resultErr == nil && closeErr != nil {
			resultErr = closeErr
		}
	}()
	block := make([]byte, clusterStorageBlock)
	var samples []time.Duration
	var offset int64
	timer := time.NewTimer(duration)
	defer timer.Stop()
	pace := time.NewTicker(max(p.pace, time.Millisecond))
	defer pace.Stop()
	for done := false; !done; {
		if _, err := p.write(file, block, offset); err != nil {
			return result, fmt.Errorf("storage probe write in %s: %w", dir, err)
		}
		started := time.Now()
		if err := p.sync(file); err != nil {
			return result, fmt.Errorf("storage probe sync in %s: %w", dir, err)
		}
		samples = append(samples, time.Since(started))
		if offset += clusterStorageBlock; offset >= clusterStorageFileLimit {
			offset = 0
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-timer.C:
			done = true
		case <-pace.C:
		}
	}
	result, err = summarizeClusterStorage(samples)
	if err != nil {
		return result, err
	}
	result.Dir = dir
	result.Filesystem, result.Device = clusterStorageMount("/proc/self/mountinfo", dir)
	return result, nil
}

// clusterStorageMount names the filesystem type and source of the longest
// mount point containing dir; both are empty when the table is unavailable.
func clusterStorageMount(table, dir string) (filesystem, device string) {
	file, err := os.Open(table)
	if err != nil {
		return "", ""
	}
	defer file.Close()
	best := -1
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// "36 35 98:0 / /mnt rw - ext4 /dev/vda1 rw": the mount point is the
		// fifth field, the type and source follow the lone "-".
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if len(fields) < 5 || separator < 0 || separator+2 >= len(fields) {
			continue
		}
		mount := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(fields[4])
		if dir != mount && mount != "/" && !strings.HasPrefix(dir, mount+"/") {
			continue
		}
		if len(mount) > best {
			best, filesystem, device = len(mount), fields[separator+1], fields[separator+2]
		}
	}
	return filesystem, device
}
