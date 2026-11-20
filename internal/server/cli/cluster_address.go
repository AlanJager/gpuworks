//go:build linux && dqlite

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/server/app"
)

func clusterAddressCommand(ctx context.Context, args []string, output io.Writer) error {
	cfg := app.DefaultConfig()
	flags := flag.NewFlagSet("cluster-address", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DataDir, "data", cfg.DataDir, "existing data directory with initialized enrollment identity")
	flags.StringVar(&cfg.DBPath, "db", cfg.DBPath, "existing native database path")
	address := flags.String("address", "", "explicit member database HOST:PORT matching its certificate")
	offline := flags.Bool("offline", false, "confirm all server processes using the database are stopped")
	timeout := flags.Duration("timeout", 5*time.Minute, "maximum offline conversion time; interrupted publication can be resumed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("cluster-address does not accept positional arguments")
	}
	if !*offline {
		return errors.New("stop the server, then pass --offline; address activation shares its runtime lock")
	}
	if *address == "" {
		return errors.New("cluster-address requires --address")
	}
	if *timeout <= 0 {
		return errors.New("cluster-address requires a positive timeout")
	}
	if err := requireLocalDatabaseAddress(*address); err != nil {
		return err
	}
	if err := useRecordedStorageTier(cfg.DataDir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	member, err := app.ActivateDatabaseAddress(ctx, cfg, *address)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{"id": member.ID, "address": member.Address, "role": member.Role})
}
