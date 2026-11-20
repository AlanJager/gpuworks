//go:build linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type clusterInstallPID struct {
	PID   int
	Start string
}

func clusterInstallProcessIdentity(pid int) (start string, alive bool, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return "", false, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return "", false, errors.New("truncated process stat")
	}
	return fields[19], fields[0] != "Z", nil
}

func clusterInstallOwnedProcess(req clusterInstallNodeRequest, kind string) (int, error) {
	raw, err := clusterInstallReadPrivate(req.Dir + "/" + kind + ".pid")
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var record clusterInstallPID
	if err := json.Unmarshal(raw, &record); err != nil {
		return 0, err
	}
	if record.PID <= 1 || record.Start == "" {
		return 0, errors.New("invalid owned PID record")
	}
	start, alive, err := clusterInstallProcessIdentity(record.PID)
	if err != nil {
		return 0, err
	}
	if !alive {
		return 0, nil
	}
	if start != record.Start {
		return 0, errors.New("recorded PID was reused; refusing to signal")
	}
	proc := fmt.Sprintf("/proc/%d/", record.PID)
	executable, err := os.Readlink(proc + "exe")
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	raw, err = os.ReadFile(proc + "cmdline")
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	matches := executable == req.Dir+"/gpuworks" && len(args) > 1 && args[1] == map[string]string{"serve": "serve", "join": "cluster-join"}[kind]
	dataMatches := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--data" && args[i+1] == req.Dir+"/data" {
			dataMatches = true
		}
	}
	if !matches || !dataMatches {
		// A process can exit between proc reads. Recheck lifecycle before rejecting
		// a live mismatch; never turn a reused or live foreign process into owned.
		now, living, err := clusterInstallProcessIdentity(record.PID)
		if err != nil {
			return 0, err
		}
		if !living {
			return 0, nil
		}
		if now != record.Start {
			return 0, errors.New("recorded PID was reused")
		}
		return 0, errors.New("live process does not belong to this installation")
	}
	return record.PID, nil
}

func clusterInstallCanStart(req clusterInstallNodeRequest) error {
	for _, kind := range []string{"serve", "join"} {
		pid, err := clusterInstallOwnedProcess(req, kind)
		if err != nil {
			return err
		}
		if pid != 0 {
			return errors.New("installation already owns a running process")
		}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--data" && args[i+1] == req.Dir+"/data" {
				return errors.New("unrecorded process uses this installation data; refusing another start")
			}
		}
	}
	// Refuse occupied listeners before starting. No existing process is stopped.
	for _, port := range []int{req.HTTP, req.Enrollment, req.Database, req.Database + 1} {
		listener, err := net.Listen("tcp", net.JoinHostPort(req.Host, strconv.Itoa(port)))
		if err != nil {
			return fmt.Errorf("installation port %d is occupied: %w", port, err)
		}
		listener.Close()
	}
	return clusterInstallPreviousExits(req)
}

func clusterInstallStart(req clusterInstallNodeRequest, join bool, args []string) error {
	return clusterInstallStartContext(context.Background(), req, join, args)
}

func clusterInstallStartContext(ctx context.Context, req clusterInstallNodeRequest, join bool, args []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := clusterInstallCanStart(req); err != nil {
		return err
	}
	kind := "serve"
	if join {
		kind = "join"
	} else {
		args = []string{"serve", "--data", req.Dir + "/data", "--db", req.Dir + "/data/gpuworks.db", "--addr", net.JoinHostPort(req.Host, strconv.Itoa(req.HTTP)), "--log-file", req.Dir + "/logs/server.log", "--recovery-enabled=false"}
	}
	// Invitations have already been validated and written to the owned token
	// file. The supervisor needs no invitation secret in its bootstrap frame.
	req.Invitation = nil
	return clusterInstallSupervisor(ctx, clusterInstallLaunch{Request: req, Kind: kind, Args: args})
}

func clusterInstallStop(ctx context.Context, req clusterInstallNodeRequest, kind string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pid, err := clusterInstallOwnedProcess(req, kind)
	if err != nil {
		return err
	}
	if pid == 0 {
		return errors.New("owned process exited before requested transition; logs retained")
	}
	raw, err := clusterInstallReadPrivate(req.Dir + "/" + kind + ".pid")
	if err != nil {
		return err
	}
	var record clusterInstallPID
	if err := json.Unmarshal(raw, &record); err != nil || record.PID != pid {
		return errors.New("owned process record changed before transition")
	}
	owner, err := clusterInstallReadOwner(req, kind, record)
	if err != nil {
		return err
	}
	parentFD, err := clusterInstallPinSupervisor(req, owner)
	if err != nil {
		return err
	}
	if parentFD >= 0 {
		defer unix.Close(parentFD)
	}
	// Pin the kernel process before the final ownership recheck: a recycled
	// numeric PID must never receive a transition intended for the old owner.
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("pin owned process (Linux pidfd support required): %w", err)
	}
	defer unix.Close(fd)
	current, err := clusterInstallOwnedProcess(req, kind)
	if err != nil {
		return err
	}
	if current != pid {
		return errors.New("owned process exited before transition; logs retained")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil {
		return err
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	receiptTick := time.NewTicker(10 * time.Millisecond)
	defer receiptTick.Stop()
	var exitErr error
	for {
		// Linux clears cmdline during exit before stat reports Z. The pinned
		// descriptor observes the process that actually received SIGTERM,
		// independent of that teardown or reuse of its numeric PID.
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		if n > 0 && fds[0].Revents&unix.POLLIN != 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			complete, err := clusterInstallReadExit(req, owner)
			if err != nil && !complete {
				return err
			}
			if complete {
				exitErr = err
				gone := parentFD < 0
				if !gone {
					parents := []unix.PollFd{{Fd: int32(parentFD), Events: unix.POLLIN}}
					_, pollErr := unix.Poll(parents, 0)
					if pollErr != nil && !errors.Is(pollErr, unix.EINTR) {
						return pollErr
					}
					if parents[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
						return errors.New("owned supervisor descriptor could not observe exit")
					}
					gone = parents[0].Revents&unix.POLLIN != 0
				}
				if gone {
					return errors.Join(exitErr, ctx.Err())
				}
			}
			// Death is not an exit status. The original stop timer includes
			// receipt publication; it must not be restarted after POLLIN.
			select {
			case <-ctx.Done():
				return errors.Join(exitErr, ctx.Err())
			case <-deadline.C:
				return errors.Join(exitErr, errors.New("owned exit status and supervisor stop were not proved within 30 seconds; logs retained"))
			case <-receiptTick.C:
			}
		}
		if n > 0 && fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return errors.New("owned process descriptor could not observe exit")
		}
		select {
		case <-ctx.Done():
			return errors.Join(exitErr, ctx.Err())
		case <-deadline.C:
			return errors.New("owned process did not stop within 30 seconds; no foreign process was killed")
		default:
		}
	}
}

func clusterInstallPinSupervisor(req clusterInstallNodeRequest, owner clusterInstallProcessOwner) (int, error) {
	start, alive, err := clusterInstallProcessIdentity(owner.Supervisor.PID)
	if err != nil {
		return -1, err
	}
	if !alive {
		return -1, nil
	}
	if start != owner.Supervisor.Start {
		return -1, errors.New("owned supervisor PID was reused")
	}
	fd, err := unix.PidfdOpen(owner.Supervisor.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	valid := false
	defer func() {
		if !valid {
			unix.Close(fd)
		}
	}()
	proc := fmt.Sprintf("/proc/%d/", owner.Supervisor.PID)
	executable, exeErr := os.Readlink(proc + "exe")
	args, argsErr := os.ReadFile(proc + "cmdline")
	now, living, err := clusterInstallProcessIdentity(owner.Supervisor.PID)
	if err != nil || living && now != owner.Supervisor.Start {
		return -1, errors.New("owned supervisor identity changed before pinning")
	}
	if living && (exeErr != nil || argsErr != nil || executable != req.Dir+"/gpuworks" || string(args) != req.Dir+"/gpuworks\x00"+clusterInstallSupervisorCommand+"\x00") {
		return -1, errors.New("live supervisor does not belong to this installation")
	}
	valid = true
	return fd, nil
}

func clusterInstallProcessSupport() error {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return fmt.Errorf("Linux pidfd support is required for owned process transitions: %w", err)
	}
	return unix.Close(fd)
}
