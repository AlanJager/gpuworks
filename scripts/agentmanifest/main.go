// Command agentmanifest records a release's explicit raw Agent build outputs.
package main

import (
	"flag"
	"fmt"
	"os"

	"gpuworks.ai/gpuworks/internal/server/agentbin"
)

func main() {
	version := flag.String("version", "", "release version embedded in the binaries")
	output := flag.String("o", "", "manifest output path")
	flag.Parse()
	if *output == "" {
		fmt.Fprintln(os.Stderr, "agentmanifest: -o is required")
		os.Exit(2)
	}
	if err := agentbin.WriteManifest(*version, *output, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "agentmanifest:", err)
		os.Exit(1)
	}
}
