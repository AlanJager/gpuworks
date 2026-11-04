//go:build !linux && !darwin

package securefile

import "os"

func groupID(info os.FileInfo) (int, bool) { return -1, false }
