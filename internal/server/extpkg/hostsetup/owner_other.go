//go:build !unix

package hostsetup

import "io/fs"

// ownerOf has no owner to report where files have no uid: no record
// counts there, and host setup is not supported.
func ownerOf(fs.FileInfo) (int, bool) { return 0, false }
