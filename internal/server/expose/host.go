package expose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// connectorTermGrace is how long the connector gets between SIGTERM and
// SIGKILL during a shutdown.
const connectorTermGrace = 10 * time.Second

// HostMain is the hidden `gpuworks expose connector-host` entry point: the
// helper that owns the connector process.
//
// It exists for one reason. The connector must not survive the server, and a
// server can die without warning — SIGKILL, a crash, the OOM killer — so the
// guarantee cannot live in the server's shutdown path. The helper watches the
// pipe on stdin instead: that pipe is held by the server, and when the server
// dies the write end closes, the helper reads EOF, and it takes the connector
// down. There is no pid guessing and no /proc inspection on either side, which
// is what makes it work on macOS too, where no parent-death signal exists.
//
// stdin is the liveness pipe, never connector input; the spec arrives in the
// environment (see HostSpecEnv) so no credential reaches a command line.
func HostMain(ctx context.Context, stdin io.Reader) int {
	// A write to fd 1 or 2 that fails with EPIPE raises SIGPIPE, whose default
	// action kills the process on the spot — before the cleanup below runs.
	// That is exactly the situation this helper exists for: the server died, so
	// its end of our stdout pipe is gone, and the first log line about stopping
	// the connector would otherwise take this process down and leave the
	// connector running. Ignored, the write just returns an error.
	signal.Ignore(syscall.SIGPIPE)

	// This helper takes a process group of its own, so that "our group" means
	// this tree and nothing else. The server already starts it in one; a manual
	// `gpuworks expose connector-host` would otherwise share the shell's job,
	// and stopping the connector would take the shell down with it. Failing to
	// move is not fatal — a session leader is already alone in its group.
	if err := syscall.Setpgid(0, 0); err != nil {
		fmt.Fprintf(os.Stderr, "connector-host: cannot take its own process group: %v\n", err)
	}

	raw := os.Getenv(HostSpecEnv)
	if raw == "" {
		fmt.Fprintln(os.Stderr, "connector-host: "+HostSpecEnv+" is not set")
		return 2
	}
	var spec Spec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		fmt.Fprintf(os.Stderr, "connector-host: bad spec: %v\n", err)
		return 2
	}
	if spec.Binary == "" {
		fmt.Fprintln(os.Stderr, "connector-host: the spec names no binary")
		return 2
	}

	cmd := exec.Command(spec.Binary, spec.Args...)
	cmd.Env = spec.Env
	// The connector's output goes straight through: the server's writer is the
	// one that rotates it and reads it, and a second copy here would only make
	// the two disagree.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	// No SysProcAttr here, deliberately: the connector inherits this helper's
	// process group instead of being given one of its own. One signal to that
	// group then covers the connector and anything it spawned — and, because a
	// process group outlives its leader, it still reaches the connector after
	// the helper is gone. A connector in a group of its own would become
	// unreachable the moment its helper died, and would go on serving a dead
	// server's address from the public internet.

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "connector-host: start %s: %v\n", spec.Binary, err)
		return 1
	}

	// Two ways this ends, and both must take the connector down: the server
	// disconnected (EOF on the pipe), or we were asked to stop (a signal from
	// the server's shutdown path).
	stopping := make(chan struct{})
	var once sync.Once
	stop := func(reason string) {
		once.Do(func() {
			fmt.Fprintf(os.Stderr, "connector-host: stopping the connector (%s)\n", reason)
			close(stopping)
		})
	}
	go watchPipe(stdin, stop)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	go func() {
		select {
		case sig := <-signals:
			stop("signal " + sig.String())
		case <-stopping:
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			stop("context cancelled")
		case <-stopping:
		}
	}()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	select {
	case err := <-waited:
		// The connector ended by itself; report how it went so the server can
		// tell a crash from a clean stop.
		return exitCode(err)
	case <-stopping:
	}

	// A connector that exits on SIGTERM does so in well under a second
	// (cloudflared closes its edge connections first). Ten seconds is the
	// grace; after that the point is to be gone, not to be polite.
	signalTree(syscall.SIGTERM)
	select {
	case err := <-waited:
		return exitCode(err)
	case <-time.After(connectorTermGrace):
	}
	// SIGKILL cannot be caught, so this ends this helper too: the connector
	// ignored a shutdown request, and there is no one left to report an exit
	// code to. The wait below is a fallback for a helper that was moved out of
	// the group it is signalling.
	signalTree(syscall.SIGKILL)
	select {
	case <-waited:
		return exitCode(errors.New("connector killed after it ignored SIGTERM"))
	case <-time.After(5 * time.Second):
		fmt.Fprintln(os.Stderr, "connector-host: the connector process group did not exit")
		return 1
	}
}

// signalTree signals this helper's process group, which the connector joined at
// launch: one signal, the whole tree the helper owns.
func signalTree(sig syscall.Signal) {
	_ = signalGroup(syscall.Getpgrp(), sig)
}

// watchPipe treats the pipe as a lease on the connector's life: EOF means the
// server is gone.
func watchPipe(stdin io.Reader, stop func(reason string)) {
	_, err := io.Copy(io.Discard, stdin)
	if err != nil {
		stop("the liveness pipe broke")
		return
	}
	stop("the server closed the liveness pipe")
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code > 0 {
			return code
		}
		// Killed by a signal.
		return 128 + int(exit.ProcessState.Sys().(syscall.WaitStatus).Signal())
	}
	fmt.Fprintf(os.Stderr, "connector-host: %v\n", err)
	return 1
}
