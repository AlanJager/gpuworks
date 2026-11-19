//go:build linux

package hostidentity

import (
	"os"
	"strings"
)

// local reads the facts from the root file system, with the gaps filled
// from RunFile.
func local() Facts { return Load(os.DirFS("/"), strings.TrimPrefix(RunFile, "/")) }
