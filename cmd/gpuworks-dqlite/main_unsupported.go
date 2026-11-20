//go:build !linux || !cgo || !dqlite

// Command gpuworks-dqlite runs the replicated database node for a clustered
// control plane. Only the Linux cgo build with the dqlite tag contains it.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gpuworks-dqlite requires linux cgo dqlite build")
	os.Exit(2)
}
