package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// Closing and removing an entry point: both are requests to the connector
// service first — the configuration is what it acts on — and only then local
// state. They live apart from the commands that ask for an entry point because
// this half is about making sure nothing is left running.

func exposeDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("expose down", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", "", "data directory (default: discovered)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	io := selfinstall.NewIO()
	store := exposeStore(*dataDir, io, false)
	cfg, err := store.ReadConfig()
	if err != nil {
		if errors.Is(err, expose.ErrNoConfig) {
			io.Say("no entry point is configured")
			return nil
		}
		return err
	}
	if cfg.Desired == expose.DesiredDisabled && !store.SupervisorHeld() && !systemdServiceInstalled() {
		io.Say("the entry point is already disabled")
		return nil
	}
	return closeEntryPoint(ctx, io, store)
}

// closeEntryPoint asks the connector service to close the entry point and
// waits until the service is gone — it exits once nothing is asked for. "down"
// means the process is no longer there, not that a file says so.
func closeEntryPoint(ctx context.Context, io *selfinstall.IO, store *expose.Store) error {
	cfg, err := store.ReadConfig()
	if err != nil {
		return err
	}
	cfg.Desired = expose.DesiredDisabled
	cfg.Revision = expose.NewRevision()
	if err := store.WriteConfig(cfg); err != nil {
		return err
	}
	if systemdServiceInstalled() {
		// The unit would only be started again; stopping it is the close.
		if err := removeSystemdService(ctx, io); err != nil {
			return err
		}
	}
	if !store.SupervisorHeld() {
		io.Say("the entry point is closed")
		return nil
	}
	io.Say("asked the connector service to close the entry point…")
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !store.SupervisorHeld() {
			io.Say("the entry point is closed")
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("the connector service has not exited; check `gpuworks expose status` and %s", store.DaemonLogPath())
}

func exposeRemove(args []string) error {
	fs := flag.NewFlagSet("expose rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", "", "data directory (default: discovered)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	io := selfinstall.NewIO()
	store := exposeStore(*dataDir, io, false)
	cfg, _ := store.ReadConfig()
	tokenFile := ""
	if cfg != nil {
		tokenFile = cfg.TokenFile
	}
	// rm is down and then removal: deleting the configuration of a running
	// entry point would leave the connector serving with nothing left to
	// describe or stop it. A server that is not hosting anything has nothing to
	// close, and then there is nothing to wait for either. A hosting lock that
	// cannot be examined leaves the question open, so nothing is removed.
	held, err := store.SupervisorCheck()
	if err != nil {
		return fmt.Errorf("cannot tell whether the connector service is running, so nothing was removed: %w; fix the lock file's owner and mode", err)
	}
	if cfg != nil && (held || systemdServiceInstalled()) {
		if err := closeEntryPoint(context.Background(), io, store); err != nil {
			return fmt.Errorf("the entry point could not be closed, so nothing was removed: %w", err)
		}
	}
	if err := store.RemoveLocal(tokenFile); err != nil {
		return err
	}
	io.Say("local configuration and observation removed")
	io.Say("kept: the connector binary cache (%s) and the log (%s)", store.BinDir(), store.LogPath())
	if tokenFile != "" && filepath.Dir(tokenFile) != store.CredentialsDir() {
		io.Say("kept: %s (it was yours, not ours)", tokenFile)
	}
	io.Say("nothing on Cloudflare was changed: the tunnel, its routes and its DNS are yours")
	return nil
}
