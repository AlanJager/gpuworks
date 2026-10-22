package selfinstall

import (
	"fmt"
	"os"
)

// lockUpgrade takes the installation's upgrade lock and returns the release
// function. Two upgrades at once would interleave their stops, backups and
// binary swaps, so the second one fails immediately rather than waiting.
func (i *Installer) lockUpgrade() (func(), error) {
	if err := mkdirAll(i.layout.Dir, dirMode); err != nil {
		return nil, err
	}
	path := i.layout.LockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	acquired, err := lockFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !acquired {
		f.Close()
		return nil, fmt.Errorf("another gpuworks upgrade is already running (lock %s)", path)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
