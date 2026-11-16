// Command gpuworks is the control-plane server.
//
// Subcommands:
//
//	serve           run the server
//	install         install the server as a system service
//	upgrade         replace the installed server with the published release
//	restart         restart the installed server so server.yaml takes effect
//	reset-password  change an admin password
//	support-bundle  collect diagnostics without the server running
//	migrate         apply database migrations and exit
//	expose          publish this server through a Cloudflare quick tunnel
//	version         print version
//
// Running the binary without a subcommand prints the usage. The command line
// itself lives in internal/server/cli.
package main

import (
	"os"

	"gpuworks.ai/gpuworks/internal/server/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:], cli.Options{})) }
