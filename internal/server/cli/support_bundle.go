package cli

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
	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// supportBundle collects diagnostics without the server: for a server that
// does not start, or whose console cannot be reached. The data directory's
// databases and the log file are read directly; nodes cannot be asked, so
// their part is missing (the manifest says so).
func (o Options) supportBundle(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var dataDir, dbPath, logFile, out, from, to, deployments, instances string
	var since time.Duration
	var local bool
	fs.StringVar(&dataDir, "data", "", "data directory (default: data_dir of server.yaml next to the binary, then of the default installation, then ./data)")
	fs.StringVar(&dbPath, "db", "", "sqlite database path (default <data>/gpuworks.db)")
	fs.StringVar(&logFile, "log-file", "", "server log file (default <data>/logs/gpuworks-server.log; on a control-plane cluster member also <installation>/logs/server.log)")
	fs.BoolVar(&local, "local", false, "on a control-plane cluster member: collect this member alone, without asking the other members")
	fs.DurationVar(&since, "since", diagnostics.DefaultBundleWindow, "collect the last DURATION (ignored with --from/--to)")
	fs.StringVar(&from, "from", "", "window start, RFC 3339")
	fs.StringVar(&to, "to", "", "window end, RFC 3339")
	fs.StringVar(&deployments, "deployments", "", "comma-separated deployment ids whose records and instance logs to include")
	fs.StringVar(&instances, "instances", "", "comma-separated instance uuids whose logs to include")
	fs.StringVar(&out, "out", "", "archive path (default ./gpuworks-diagnostics-<id>.tar.gz)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	req := diagnostics.BundleRequest{RequestedBy: "support-bundle"}
	var err error
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
	if dbPath == "" {
		// A control-plane cluster member keeps no gpuworks.db: its part is
		// its files, and the other members are asked for theirs.
		dir, _ := expose.ResolveDataDir(dataDir)
		if member, err := selfinstall.ClusterMember(dir); err == nil && member {
			return clusterMemberSupportBundle(ctx, log, dir, logFile, out, req, local)
		}
	}
	if local {
		return fmt.Errorf("--local applies to a control-plane cluster member; this data directory is a single server's")
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
	bundle, archive, err := app.CollectDiagnosticsOffline(ctx, app.OfflineDiagnosticsRequest{DataDir: dataDir, DBPath: db, LogFile: logFile, WorkDir: work, Request: req, Extensions: o.Extensions}, log, policy)
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

// clusterMemberSupportBundle collects on a control-plane cluster member: its
// own log, stall reports and database role records, and the other members'
// over their member route. The cluster database is not opened, so this works
// while the cluster has no leader or this member's server is down.
func clusterMemberSupportBundle(ctx context.Context, log *slog.Logger, dataDir, logFile, out string, req diagnostics.BundleRequest, local bool) error {
	if len(req.DeploymentIDs) > 0 || len(req.InstanceUUIDs) > 0 {
		return fmt.Errorf("--deployments and --instances read the cluster database, which support-bundle does not open on a cluster member; collect them from the console (Settings > Diagnostics) while the cluster serves")
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	if logFile == "" {
		logFile = memberLogFile(dataDir)
	}
	outDir := "."
	if out != "" {
		outDir = filepath.Dir(out)
	}
	work, err := os.MkdirTemp(outDir, ".gpuworks-diagnostics-*")
	if err != nil {
		return fmt.Errorf("stage the collection in %s: %w", outDir, err)
	}
	defer os.RemoveAll(work)
	bundle, archive, err := app.CollectClusterMemberDiagnostics(ctx, app.ClusterMemberDiagnosticsRequest{DataDir: dataDir, LogFile: logFile, WorkDir: work, Request: req, Local: local}, log)
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
	for _, item := range bundle.Items {
		if item.ID() == diagnostics.BundleItemMember {
			fmt.Fprintf(os.Stderr, "support-bundle: %s is missing (%s); the collection does not show what that member saw\n", item.Path, item.Evidence)
		}
	}
	fmt.Println(out)
	return nil
}

// memberLogFile is a cluster member's server log: the default beside the
// data directory, or where the cluster installation keeps it
// (<installation>/logs/server.log, with data in <installation>/data).
func memberLogFile(dataDir string) string {
	candidates := []string{defaultLogFile(dataDir), filepath.Join(filepath.Dir(dataDir), "logs", "server.log"), filepath.Join(filepath.Dir(dataDir), "logs", "gpuworks-server.log")}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return candidates[0]
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
