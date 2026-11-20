//go:build linux && dqlite

package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/server/app"
)

func statusCommand(ctx context.Context, args []string, output io.Writer) error {
	cfg := app.DefaultConfig()
	flags := flag.NewFlagSet("cluster-status", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory")
	flags.StringVar(&cfg.DBPath, "db", cfg.DBPath, "database path")
	httpAddr := flags.String("http-addr", "", "local HTTP address, when it is not recorded in the data directory")
	timeout := flags.Duration("timeout", 5*time.Second, "deadline for one listener, peer, or quorum probe")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("cluster-status does not accept positional arguments")
	}
	if *timeout <= 0 {
		return errors.New("cluster-status requires a positive timeout")
	}
	if err := useRecordedStorageTier(cfg.DataDir); err != nil {
		return err
	}
	report, observationErr := app.ObserveClusterStatus(ctx, cfg, *httpAddr, *timeout)
	if report != "" {
		if _, err := io.WriteString(output, report); err != nil {
			return err
		}
	}
	return observationErr
}
