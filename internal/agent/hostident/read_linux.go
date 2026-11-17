//go:build linux

package hostident

import "os"

func read() string { return readDMI(os.DirFS("/")) }
