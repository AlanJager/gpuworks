// gpuworks-dist builds and checks the distribution channel tree (ADR 0040).
//
//	assemble    turn release archives and the embedded catalogs into a signed tree
//	release-id  print the release id of the archives in a dist directory
//	adopt       check a release directory copied back from a source
//	verify      read a channel (or a candidate index) back from published sources
//	            the way servers do
//	newer       tell whether a source already serves a newer build of a channel
//	prune       pick old release directories a source no longer needs
//	notices     sign the publisher's notices, or read them back from sources
//
// Uploading is left to scripts/publish_dist.sh (rclone), so this tool never
// holds storage credentials; it only needs the signing key for assemble and notices.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "assemble":
		err = assembleCommand(args[1:], stdout)
	case "release-id":
		err = releaseIDCommand(args[1:], stdout)
	case "adopt":
		err = adoptCommand(args[1:], stdout)
	case "verify":
		err = verifyCommand(args[1:], stdout)
	case "newer":
		// Exit 0 means a newer build is published and 1 that none is; any
		// other failure is 2, so a script branching on 0 publishes then.
		err = newerCommand(args[1:], stdout)
		if errors.Is(err, errNotNewer) {
			return 1
		}
		if err != nil {
			fmt.Fprintf(stderr, "gpuworks-dist newer: %v\n", err)
			return 2
		}
	case "prune":
		err = pruneCommand(args[1:], stdin, stdout)
	case "notices":
		err = noticesCommand(args[1:], stdout)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n%s\n", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "gpuworks-dist %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

const usage = `usage:
  gpuworks-dist assemble -channel nightly -dist DIR -out DIR [-commit SHA] [-built-at TIME]
  gpuworks-dist release-id -dist DIR
  gpuworks-dist adopt -dir DIR -release-id ID   (prints the archives' build time)
  gpuworks-dist verify -channel nightly -source URL [-source URL ...] [-release-id ID] [-index FILE] [-full]
  gpuworks-dist newer -channel nightly -built-at TIME [-source URL ...]   (exit 0: a newer build is published)
  gpuworks-dist prune -keep 60 [-protect ID ...] < rclone-lsjson-of-releases.json
  gpuworks-dist notices -in build/notices/notices.json -out FILE
  gpuworks-dist notices -source URL [-source URL ...] [-expect FILE]`

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, strings.TrimSpace(value))
	return nil
}
