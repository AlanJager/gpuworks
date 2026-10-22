//go:build !linux || (!amd64 && !arm64)

package assets

import "embed"

// files and perftestFiles are empty on targets that cannot run these Linux
// GPU assets. A darwin agent is one of those targets.
var files embed.FS
var perftestFiles embed.FS
