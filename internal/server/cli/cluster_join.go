//go:build linux && dqlite

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/app"
)

func clusterJoinCommand(ctx context.Context, args []string, output io.Writer) error {
	cfg := app.DefaultConfig()
	flags := flag.NewFlagSet("cluster-join", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory containing the prepared join identity")
	flags.StringVar(&cfg.DBPath, "db", cfg.DBPath, "managed database path")
	address := flags.String("address", "", "this member's database HOST:PORT")
	clusterID := flags.String("cluster-id", "", "expected cluster id")
	caFingerprint := flags.String("ca-fingerprint", "", "expected cluster CA fingerprint")
	enrollmentURL := flags.String("enrollment-url", "", "https://HOST:PORT enrollment origin")
	publicURL := flags.String("public-url", "", "this member's administrator-authorized HTTPS origin")
	publicListen := flags.String("public-listen", "", "local IP:PORT for this member's HTTPS material receiver")
	tokenFile := flags.String("token-file", "", "private invitation token file (required for a fresh join; omit after admission on retry)")
	timeout := flags.Duration("timeout", 5*time.Minute, "maximum time to join and observe membership")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("cluster-join does not accept positional arguments")
	}
	if *address == "" || *clusterID == "" || *caFingerprint == "" || *enrollmentURL == "" {
		return errors.New("cluster-join requires --address, --cluster-id, --ca-fingerprint, and --enrollment-url")
	}
	if *timeout <= 0 {
		return errors.New("cluster-join requires a positive timeout")
	}
	token, err := readClusterJoinToken(*tokenFile)
	if err != nil {
		return err
	}
	if token == "" {
		if _, err := os.Lstat(filepath.Join(cfg.DataDir, "gpuworks.db.join")); errors.Is(err, os.ErrNotExist) {
			return errors.New("cluster-join requires --token-file for a new join")
		} else if err != nil {
			return err
		}
	}
	if err := requireLocalDatabaseAddress(*address); err != nil {
		return err
	}
	joinCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	target := app.ClusterJoinTarget{
		ClusterID: *clusterID, CAFingerprint: *caFingerprint, Protocol: proto.ClusterProtocolVersion,
		EnrollmentURL: *enrollmentURL, EnrollmentOrigin: *publicURL, EnrollmentListen: *publicListen, Address: *address,
	}
	if _, err := app.PrepareClusterJoin(joinCtx, cfg, target); err != nil {
		return fmt.Errorf("prepare cluster join: %w", err)
	}
	if _, err := app.ReceiveClusterMaterials(joinCtx, cfg, target, token); err != nil {
		return fmt.Errorf("receive cluster materials: %w", err)
	}
	if err := app.ReceiveClusterTransportPolicy(joinCtx, cfg, target); err != nil {
		return fmt.Errorf("receive cluster transport policy: %w", err)
	}
	if err := app.ReceiveClusterCatalog(joinCtx, cfg, target); err != nil {
		return fmt.Errorf("receive cluster catalog: %w", err)
	}
	if err := app.VerifyClusterJoinTransport(joinCtx, cfg, target); err != nil {
		return fmt.Errorf("verify cluster transport: %w", err)
	}
	if err := app.RegisterClusterJoinOrigin(joinCtx, cfg, target); err != nil {
		return fmt.Errorf("register cluster public origin: %w", err)
	}
	session, observed, err := app.JoinCluster(joinCtx, cfg, target)
	if err != nil {
		return fmt.Errorf("join cluster database: %w", err)
	}
	return clusterJoinWait(ctx, output, observed, session.Close)
}

func readClusterJoinToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("cluster-join token file must be a private regular file")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return "", errors.New("cluster-join token file must belong to the current user")
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return "", errors.New("cluster-join token file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(raw) > 4096 {
		return "", errors.New("cluster-join token file is too large")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("cluster-join token file contains no single token")
	}
	return token, nil
}
