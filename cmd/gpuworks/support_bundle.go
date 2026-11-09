package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// supportBundle collects diagnostics without the server: for a server that
// does not start, or whose console cannot be reached. The data directory's
// databases and the log file are read directly; nodes cannot be asked, so
// their part is missing (the manifest says so).
func supportBundle(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var dataDir, dbPath, logFile, out, from, to, deployments, instances string
	var since time.Duration
	fs.StringVar(&dataDir, "data", "", "data directory (default: data_dir of server.yaml next to the binary, then of the default installation, then ./data)")
	fs.StringVar(&dbPath, "db", "", "sqlite database path (default <data>/gpuworks.db)")
	fs.StringVar(&logFile, "log-file", "", "server log file (default <data>/logs/gpuworks-server.log)")
	fs.DurationVar(&since, "since", diagnostics.DefaultBundleWindow, "collect the last DURATION (ignored with --from/--to)")
	fs.StringVar(&from, "from", "", "window start, RFC 3339")
	fs.StringVar(&to, "to", "", "window end, RFC 3339")
	fs.StringVar(&deployments, "deployments", "", "comma-separated deployment ids whose records and instance logs to include")
	fs.StringVar(&instances, "instances", "", "comma-separated instance uuids whose logs to include")
	fs.StringVar(&out, "out", "", "archive path (default ./gpuworks-diagnostics-<id>.tar.gz)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := resetPasswordDB(dataDir, dbPath)
	if err != nil {
		return err
	}
	dbFile, err := store.DatabaseFilePath(db)
	if err != nil {
		return err
	}
	if dbFile == "" {
		return fmt.Errorf("support-bundle requires a persistent database")
	}
	dataDir = filepath.Dir(dbFile)
	if logFile == "" {
		logFile = defaultLogFile(dataDir)
	}
	req := diagnostics.BundleRequest{RequestedBy: "support-bundle"}
	if from != "" || to != "" {
		if req.From, err = time.Parse(time.RFC3339, from); err != nil {
			return fmt.Errorf("--from: %w", err)
		}
		if req.To, err = time.Parse(time.RFC3339, to); err != nil {
			return fmt.Errorf("--to: %w", err)
		}
	} else {
		req.To = time.Now()
		req.From = req.To.Add(-since)
	}
	for _, field := range strings.Split(deployments, ",") {
		if field = strings.TrimSpace(field); field == "" {
			continue
		}
		id, err := strconv.ParseUint(field, 10, 64)
		if err != nil || id == 0 {
			return fmt.Errorf("--deployments: %q is not a deployment id", field)
		}
		req.DeploymentIDs = append(req.DeploymentIDs, uint(id))
	}
	for _, field := range strings.Split(instances, ",") {
		if field = strings.TrimSpace(field); field != "" {
			req.InstanceUUIDs = append(req.InstanceUUIDs, field)
		}
	}
	policy, err := maintenanceDatabasePolicy(db)
	if err != nil {
		return err
	}
	// Stage beside the output, not in the data directory: the command is what
	// is left when the server's disk is full, and --out can name another one.
	outDir := "."
	if out != "" {
		outDir = filepath.Dir(out)
	}
	work, err := os.MkdirTemp(outDir, ".gpuworks-diagnostics-*")
	if err != nil {
		return fmt.Errorf("stage the collection in %s: %w", outDir, err)
	}
	defer os.RemoveAll(work)
	bundle, archive, err := app.CollectDiagnosticsOffline(ctx, app.OfflineDiagnosticsRequest{DataDir: dataDir, DBPath: db, LogFile: logFile, WorkDir: work, Request: req}, log, policy)
	if err != nil {
		return err
	}
	if out == "" {
		out = "gpuworks-diagnostics-" + bundle.ID + ".tar.gz"
	}
	if err := os.Rename(archive, out); err != nil {
		if err := copyArchive(archive, out); err != nil {
			return err
		}
	}
	fmt.Println(out)
	return nil
}

func copyArchive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
