//go:build !linux

package engine

import "errors"

// filesystemType has no portable answer off Linux, where XFS project quotas do
// not apply anyway. Reporting the absence keeps the caller on the one path that
// always holds the limit: the sweeper.
func filesystemType(string) (int64, error) {
	return 0, errors.New("filesystem type is only available on linux")
}
