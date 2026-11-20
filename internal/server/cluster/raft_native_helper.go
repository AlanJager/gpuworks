//go:build linux && dqlite

package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
)

func generateMemberID(ctx context.Context, address string) (uint64, error) {
	out, err := runHelper(ctx, "id", "--address", address)
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("dqlite helper id: %w", err)
	}
	return id, nil
}

func reconfigureSingleVoter(ctx context.Context, dir string, info client.NodeInfo) error {
	_, err := runHelper(ctx, "reconfigure", "--dir", dir, "--id", strconv.FormatUint(info.ID, 10), "--address", info.Address, "--role", info.Role.String())
	return err
}

func describeLastRaftEntry(ctx context.Context, dir string) error {
	_, err := runHelper(ctx, "describe-last", "--dir", dir)
	return err
}

// inspectIsolatedConversionSource asks the helper to read membership and the
// schema digest. dqlite accepts an abstract-unix client only when its pid is
// the helper's own pid, so this process cannot dial that socket itself.
func inspectIsolatedConversionSource(ctx context.Context, dir, name string, info client.NodeInfo) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := runHelper(ctx, "inspect",
		"--dir", dir,
		"--id", strconv.FormatUint(info.ID, 10),
		"--address", info.Address,
		"--name", name)
	if err != nil {
		return "", err
	}
	var result struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("dqlite helper inspect: %w", err)
	}
	if result.Schema == "" {
		return "", errors.New("dqlite helper inspect returned an empty schema")
	}
	return result.Schema, nil
}

// openIsolatedNode starts a node that binds only a private socket and refuses
// every outbound Raft connection. The returned function stops it and waits.
func openIsolatedNode(ctx context.Context, id uint64, address, dir, bind, reject string) (string, func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	cmd, err := helperCommand(nil, "isolated", "--dir", dir, "--id", strconv.FormatUint(id, 10), "--address", address, "--bind", bind, "--reject", reject)
	if err != nil {
		return "", nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	notes := &cappedStderr{}
	cmd.Stderr = notes
	process, err := spawnHelper(cmd)
	if err != nil {
		return "", nil, err
	}
	var info struct {
		Bind string `json:"bind"`
	}
	line := make(chan error, 1)
	go func() { line <- json.NewDecoder(bufio.NewReader(stdout)).Decode(&info) }()
	timer := time.NewTimer(helperReadyWait)
	defer timer.Stop()
	var readErr error
	select {
	case <-ctx.Done():
		readErr = ctx.Err()
	case <-timer.C:
		readErr = errors.New("dqlite helper did not report its socket in time")
	case err := <-line:
		if err != nil {
			readErr = fmt.Errorf("dqlite helper did not report its socket: %w", err)
		} else if info.Bind == "" {
			readErr = errors.New("dqlite helper reported an empty socket")
		}
	}
	if readErr != nil {
		process.discard()
		if text := strings.TrimSpace(notes.String()); text != "" {
			readErr = fmt.Errorf("%w: %s", readErr, text)
		}
		return "", nil, readErr
	}
	process.release()
	return info.Bind, func() error { return process.stop(helperStopWait) }, nil
}
