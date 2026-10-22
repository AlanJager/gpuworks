package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

// errNotNewer is the "nothing newer is published" outcome of newer, reported
// as exit status 1 so a script can branch on it.
var errNotNewer = fmt.Errorf("no source serves a newer build")

// newerCommand answers whether any source already serves a build of the
// channel created after builtAt. Pipelines can finish out of order; the
// publish script asks this first so an older pipeline never moves a channel
// back.
func newerCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("newer", flag.ContinueOnError)
	var channel, builtAt string
	var sources stringList
	fs.StringVar(&channel, "channel", "", "channel to read")
	fs.StringVar(&builtAt, "built-at", "", "this build's time (RFC3339)")
	fs.Var(&sources, "source", "source base URL (repeatable); default: the built-in sources")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ours, err := time.Parse(time.RFC3339, strings.TrimSpace(builtAt))
	if err != nil {
		return fmt.Errorf("-built-at: %w", err)
	}
	if len(sources) == 0 {
		sources = distribution.DefaultSources
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	newer, err := publishedNewer(ctx, &distribution.Client{Sources: sources}, channel, ours)
	if err != nil {
		return err
	}
	if newer == "" {
		return errNotNewer
	}
	fmt.Fprintln(stdout, newer)
	return nil
}

// publishedNewer asks every source on its own and describes the first one
// serving a build of channel newer than ours; empty when none does. A source
// that does not answer, or a channel not published yet, is not newer.
func publishedNewer(ctx context.Context, client *distribution.Client, channel string, ours time.Time) (string, error) {
	for _, source := range client.Sources {
		one := *client
		one.Sources = []string{source}
		got, err := one.FetchIndex(ctx, channel)
		if err != nil {
			continue
		}
		theirs, err := time.Parse(time.RFC3339, got.Index.Server.BuiltAt)
		if err == nil && theirs.After(ours) {
			return fmt.Sprintf("%s serves %s built %s", source, got.Index.Server.ReleaseID, got.Index.Server.BuiltAt), nil
		}
	}
	return "", nil
}
