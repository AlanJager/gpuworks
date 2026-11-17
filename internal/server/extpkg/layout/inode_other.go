//go:build !unix

package layout

import "io/fs"

// inodeOf has no inode to offer here; size and modification time remain.
func inodeOf(fs.FileInfo) uint64 { return 0 }
