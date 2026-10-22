//go:build !unix

package launchd

import "io/fs"

func fileUID(fs.FileInfo) (int, bool) { return 0, false }
