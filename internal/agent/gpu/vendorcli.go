package gpu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// vendorCLITimeout bounds one call of a vendor CLI (nvidia-smi, npu-smi,
// hy-smi, ppu-smi, dcgmi).
var vendorCLITimeout = 10 * time.Second

// vendorCLIReapGrace is how long a killed CLI may take to exit before the
// caller stops waiting for it. A CLI blocked inside a driver ioctl sits in
// uninterruptible sleep, where SIGKILL stays pending until the driver
// returns, and exec.Cmd.Wait blocks for exactly that long.
var vendorCLIReapGrace = 3 * time.Second

// ErrCLIUnresponsive marks a vendor CLI that did not answer in time, or whose
// previous call has still not exited. It says nothing about which fields or
// subcommands the driver supports, so callers must not treat it as a reason
// to fall back to a smaller query.
var ErrCLIUnresponsive = errors.New("vendor CLI unresponsive")

// IsCLIUnresponsive reports whether err came from a timed-out or stuck CLI.
func IsCLIUnresponsive(err error) bool { return errors.Is(err, ErrCLIUnresponsive) }

// stuckCLIs records CLIs whose killed process has not exited. While one is
// stuck, new calls of the same binary fail at once: each further process
// would block on the same driver lock, and a host under trouble would gather
// one more unkillable process every few seconds.
var stuckCLIs = struct {
	sync.Mutex
	since map[string]time.Time
}{since: map[string]time.Time{}}

// RunVendorCLI runs name with args and returns its combined output. It never
// blocks the caller much past vendorCLITimeout (or ctx), even when the
// process cannot be killed.
func RunVendorCLI(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	stuckCLIs.Lock()
	since, stuck := stuckCLIs.since[name]
	stuckCLIs.Unlock()
	if stuck {
		return "", fmt.Errorf("%s: a previous call has not exited for %s: %w", name, time.Since(since).Round(time.Second), ErrCLIUnresponsive)
	}

	runCtx, cancel := context.WithTimeout(ctx, vendorCLITimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Stdin = stdin
	killGroupOnCancel(cmd)
	cmd.Stdout = &out
	cmd.Stderr = &out
	// Bounds the wait for output pipes held open by a child of the CLI.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	select {
	case err = <-done:
	case <-runCtx.Done():
		select {
		case err = <-done:
		case <-time.After(vendorCLIReapGrace):
			markCLIStuck(name, args, done)
			return "", unresponsiveError(ctx, name)
		}
	}
	if runCtx.Err() != nil {
		return out.String(), unresponsiveError(ctx, name)
	}
	return out.String(), err
}

// unresponsiveError names why a call was cut short: the caller's own
// cancellation passes through unchanged, the CLI's time limit becomes
// ErrCLIUnresponsive.
func unresponsiveError(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%s: no answer within %s: %w", name, vendorCLITimeout, ErrCLIUnresponsive)
}

func markCLIStuck(name string, args []string, done <-chan error) {
	start := time.Now()
	stuckCLIs.Lock()
	stuckCLIs.since[name] = start
	stuckCLIs.Unlock()
	slog.Warn("gpu command did not exit after kill; further calls fail fast until it does",
		"command", name, "args", strings.Join(args, " "))
	go func() {
		<-done
		stuckCLIs.Lock()
		delete(stuckCLIs.since, name)
		stuckCLIs.Unlock()
		slog.Info("stuck gpu command exited", "command", name, "stuck_for", time.Since(start).Round(time.Second))
	}()
}
