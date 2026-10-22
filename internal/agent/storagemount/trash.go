package storagemount

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func trashDir(root string) string { return filepath.Join(root, MetaDirName, TrashDirName) }

// TrashPath is where a directory removed from a shared storage is renamed to
// before its bytes are deleted. The name starts with the removal time so a
// later sweep can tell abandoned entries from ones still being deleted.
func TrashPath(root, name string, now time.Time) string {
	return filepath.Join(trashDir(root), fmt.Sprintf("%d-%s", now.UnixNano(), filepath.Base(name)))
}

// SweepTrash deletes trash entries renamed more than olderThan before now:
// leftovers of a removal whose agent restarted, or whose files were still open
// at the time. Entries whose name carries no time are swept too. It returns
// how many entries were removed and the first error met; a missing trash
// directory is not an error. These calls touch a shared path, so callers bound
// them with a deadline.
func SweepTrash(root string, olderThan time.Duration, now time.Time) (int, error) {
	ents, err := os.ReadDir(trashDir(root))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var first error
	removed := 0
	for _, e := range ents {
		stamp, _, _ := strings.Cut(e.Name(), "-")
		if ns, err := strconv.ParseInt(stamp, 10, 64); err == nil && now.Sub(time.Unix(0, ns)) < olderThan {
			continue
		}
		if err := os.RemoveAll(filepath.Join(trashDir(root), e.Name())); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		removed++
	}
	return removed, first
}
