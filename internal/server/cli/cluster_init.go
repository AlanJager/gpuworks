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
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

func clusterInitCommand(ctx context.Context, args []string, output io.Writer) error {
	cfg := app.DefaultConfig()
	flags := flag.NewFlagSet("cluster-init", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DataDir, "data", cfg.DataDir, "existing data directory")
	flags.StringVar(&cfg.DBPath, "db", cfg.DBPath, "existing native database path")
	listen := flags.String("listen", "", "dedicated enrollment TLS listen IP:port")
	publicURL := flags.String("public-url", "", "HTTPS enrollment origin reachable by joining machines")
	offline := flags.Bool("offline", false, "confirm all server processes using the database are stopped")
	tierName := flags.String("storage-tier", "", "timing profile of the cluster: standard (default) or slow-storage; every member adopts it when it joins")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("cluster-init does not accept positional arguments")
	}
	if !*offline {
		return errors.New("stop the server, then pass --offline; initialization shares its runtime lock")
	}
	if *listen == "" || *publicURL == "" {
		return errors.New("cluster-init requires --listen and --public-url")
	}
	tier, err := parseStorageTierFlag(*tierName)
	if err != nil {
		return err
	}
	if tier == "" {
		tier = storagetier.Standard
	}
	// The single-member database is opened once more before the tier is
	// recorded; from then on every start of this installation uses it.
	cluster.UseStorageTier(tier)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	info, err := app.InitializeEnrollmentWithTier(ctx, cfg, *listen, *publicURL, tier)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(info)
}
