//go:build linux

package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// All values already exist in the installer. This frame transfers them through
// a private inherited pipe; neither the frame nor credentials are persisted.
type clusterInstallLaunch struct {
	Request clusterInstallNodeRequest
	Kind    string
	Args    []string
}

func clusterInstallSupervisor(ctx context.Context, launch clusterInstallLaunch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(launch)
	if err != nil || len(raw) > 65536 {
		return errors.New("installation process bootstrap is too large")
	}
	jobRead, jobWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer jobRead.Close()
	defer jobWrite.Close()
	ackRead, ackWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer ackRead.Close()
	defer ackWrite.Close()
	decisionRead, decisionWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer decisionRead.Close()
	defer decisionWrite.Close()
	log, err := os.OpenFile(launch.Request.Dir+"/logs/process.out", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer null.Close()
	c := exec.Command(launch.Request.Dir+"/gpuworks", clusterInstallSupervisorCommand)
	c.Dir = launch.Request.Dir
	c.Stdin, c.Stdout, c.Stderr = null, log, log
	c.ExtraFiles = []*os.File{jobRead, ackWrite, decisionRead}
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Env = clusterInstallProcessEnvironment("")
	if err := c.Start(); err != nil {
		return err
	}
	// A detached supervisor survives the invoking SSH helper. Reap it if this
	// caller is still alive, without tying its lifetime to the caller context.
	go func() { _ = c.Wait() }()
	jobRead.Close()
	ackWrite.Close()
	decisionRead.Close()
	written := make(chan error, 1)
	go func() {
		_, err := jobWrite.Write(raw)
		written <- errors.Join(err, jobWrite.Close())
	}()
	acknowledged := make(chan error, 1)
	go func() {
		var owner clusterInstallProcessOwner
		decoder := json.NewDecoder(io.LimitReader(ackRead, 8193))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&owner)
		if err == nil && decoder.Decode(new(any)) != io.EOF {
			err = errors.New("invalid supervisor acknowledgment")
		}
		if err == nil {
			if owner.Supervisor.PID != c.Process.Pid {
				err = errors.New("acknowledgment belongs to another supervisor")
			} else if actual, e := clusterInstallReadOwner(launch.Request, launch.Kind, owner.clusterInstallPID); e != nil || actual != owner {
				err = errors.New("supervisor acknowledgment has no matching durable ownership")
			} else if pid, e := clusterInstallOwnedProcess(launch.Request, launch.Kind); e != nil || pid != owner.PID {
				err = errors.New("installation child exited or changed before acknowledgment")
			}
		}
		acknowledged <- err
	}()
	for written != nil || acknowledged != nil {
		select {
		case err := <-written:
			if err != nil {
				return fmt.Errorf("installation bootstrap failed; owned files retained: %w", err)
			}
			written = nil
		case err := <-acknowledged:
			if err != nil {
				return fmt.Errorf("installation supervisor did not acknowledge ownership; logs retained: %w", err)
			}
			acknowledged = nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Ownership is durable before commitment. EOF before this byte makes the
	// supervisor kill and reap its exact fresh child; no caller can advance.
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = decisionWrite.Write([]byte{1})
	closeErr := decisionWrite.Close()
	return errors.Join(err, closeErr, ctx.Err())
}

func clusterInstallProcessEnvironment(password string) []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GPUWORKS_") {
			env = append(env, entry)
		}
	}
	if password != "" {
		env = append(env, "GPUWORKS_ADMIN_PASSWORD="+password)
	}
	return env
}

func clusterInstallSupervisorCommandRun() error {
	job := os.NewFile(3, "installation bootstrap")
	ack := os.NewFile(4, "installation acknowledgment")
	decision := os.NewFile(5, "installation commitment")
	defer job.Close()
	defer ack.Close()
	defer decision.Close()
	for _, f := range []*os.File{job, ack, decision} {
		// ExtraFiles arrive without CLOEXEC. Only the supervisor may hold these
		// bootstrap descriptors; its server child must not retain or forge ACKs.
		unix.CloseOnExec(int(f.Fd()))
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return errors.New("supervisor requires inherited bootstrap pipes")
		}
	}
	var launch clusterInstallLaunch
	decoder := json.NewDecoder(io.LimitReader(job, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&launch); err != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid installation supervisor bootstrap")
	}
	job.Close()
	req := launch.Request
	if err := clusterInstallValidateLaunch(launch); err != nil {
		return err
	}
	if err := os.Mkdir(req.Dir+"/processes", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := clusterInstallPrivateDir(req.Dir + "/processes"); err != nil {
		return err
	}
	fd, err := unix.Open(req.Dir+"/processes/launch.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(fd), "installation launch lock")
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil {
		return err
	}
	lockStat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || lockStat.Uid != uint32(os.Geteuid()) || lockStat.Nlink != 1 {
		return errors.New("invalid installation launch lock")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("another installation launch is in progress")
	}
	if err := clusterInstallCanStart(req); err != nil {
		return err
	}
	boot, err := clusterInstallBootID()
	if err != nil {
		return err
	}
	executable, err := clusterInstallExecutableHash(req)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	parentStart, _, err := clusterInstallProcessIdentity(os.Getpid())
	if err != nil {
		return err
	}
	log, err := os.OpenFile(req.Dir+"/logs/"+launch.Kind+".out", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer null.Close()
	c := exec.Command(req.Dir+"/gpuworks", launch.Args...)
	c.Dir = req.Dir
	c.Stdin, c.Stdout, c.Stderr = null, log, log
	c.Env = clusterInstallProcessEnvironment(req.Password)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return err
	}
	start, alive, err := clusterInstallProcessIdentity(c.Process.Pid)
	if err != nil || !alive {
		_ = c.Process.Kill()
		waitErr := c.Wait()
		return fmt.Errorf("installation child exited before ownership: %w", waitErr)
	}
	owner := clusterInstallProcessOwner{clusterInstallPID: clusterInstallPID{c.Process.Pid, start}, Supervisor: clusterInstallPID{os.Getpid(), parentStart}, Boot: boot, Nonce: hex.EncodeToString(nonce), Kind: launch.Kind, ExecutableHash: executable}
	if err := clusterInstallPublishOwner(req, owner); err != nil {
		_ = c.Process.Kill()
		_ = clusterInstallWaitChild(req, owner, c)
		return err
	}
	if err := json.NewEncoder(ack).Encode(owner); err != nil {
		_ = c.Process.Kill()
		_ = clusterInstallWaitChild(req, owner, c)
		return err
	}
	ack.Close()
	committed, decisionErr := io.ReadAll(io.LimitReader(decision, 2))
	decision.Close()
	if decisionErr != nil || len(committed) != 1 || committed[0] != 1 {
		_ = c.Process.Kill()
		_ = clusterInstallWaitChild(req, owner, c)
		return errors.New("installation caller did not commit the acknowledged launch")
	}
	lock.Close()
	return clusterInstallWaitChild(req, owner, c)
}

func clusterInstallPublishOwner(req clusterInstallNodeRequest, owner clusterInstallProcessOwner) error {
	raw, _ := json.Marshal(owner)
	if err := clusterInstallPublishRecord(clusterInstallOwnerPath(req, owner.Nonce, "owner"), raw, false); err != nil {
		return err
	}
	if err := clusterInstallPublishRecord(clusterInstallOwnerPath(req, owner.Nonce, "pending"), raw, false); err != nil {
		return err
	}
	if err := clusterInstallPublishRecord(req.Dir+"/"+owner.Kind+".launch", []byte(owner.Nonce), true); err != nil {
		return err
	}
	raw, _ = json.Marshal(owner.clusterInstallPID)
	return clusterInstallPublishRecord(req.Dir+"/"+owner.Kind+".pid", raw, true)
}

func clusterInstallWaitChild(req clusterInstallNodeRequest, owner clusterInstallProcessOwner, c *exec.Cmd) error {
	waitErr := c.Wait()
	receipt := clusterInstallProcessExit{Owner: owner}
	if c.ProcessState != nil {
		if status, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok {
			raw := uint32(status)
			receipt.Status = &raw
		}
	}
	if err := clusterInstallPublishExit(req, receipt, clusterInstallSyncRecordDir); err != nil {
		return err
	}
	return waitErr
}

func clusterInstallPublishExit(req clusterInstallNodeRequest, receipt clusterInstallProcessExit, syncDirectory func(string) error) error {
	owner := receipt.Owner
	raw, _ := json.Marshal(receipt)
	if err := clusterInstallPublishRecordSync(clusterInstallOwnerPath(req, owner.Nonce, "exit"), raw, false, syncDirectory); err != nil {
		return fmt.Errorf("publish terminal wait status: %w", err)
	}
	// A visible rename alone is not proof of successful durable publication.
	// Retain the pending marker on every file/directory fsync failure.
	pendingPath := clusterInstallOwnerPath(req, owner.Nonce, "pending")
	pendingRaw, pendingErr := clusterInstallReadPrivate(pendingPath)
	var pendingOwner clusterInstallProcessOwner
	if pendingErr != nil || clusterInstallDecodeRecord(pendingRaw, &pendingOwner) != nil || pendingOwner != owner {
		return errors.Join(errors.New("terminal publication pending marker was lost or altered"), pendingErr)
	}
	if err := os.Remove(pendingPath); err != nil {
		return fmt.Errorf("complete terminal wait status publication: %w", err)
	}
	return nil
}

func clusterInstallValidateLaunch(launch clusterInstallLaunch) error {
	req := launch.Request
	if err := validateClusterInstallDir(req.Dir); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(req.Dir)
	if err != nil || root != req.Dir {
		return errors.New("supervisor installation directory contains a symlink")
	}
	if err := clusterInstallPrivateDir(root); err != nil {
		return err
	}
	account, err := user.Current()
	if err != nil || account.Username != req.User {
		return errors.New("supervisor account differs from installation account")
	}
	self, err := os.Executable()
	if err != nil || self != root+"/gpuworks" {
		return errors.New("supervisor must use the owned installation executable")
	}
	raw, err := clusterInstallReadPrivate(root + "/cluster-install.json")
	if err != nil {
		return err
	}
	var saved clusterInstallNodeRequest
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Dir != req.Dir || saved.Host != req.Host || saved.User != req.User || saved.HTTP != req.HTTP || saved.Enrollment != req.Enrollment || saved.Database != req.Database || saved.NoService != req.NoService || strings.Join(saved.Hosts, ",") != strings.Join(req.Hosts, ",") {
		return errors.New("supervisor installation ownership settings differ")
	}
	command := map[string]string{"serve": "serve", "join": "cluster-join"}[launch.Kind]
	if command == "" || len(launch.Args) < 3 || launch.Args[0] != command {
		return errors.New("invalid supervised installation command")
	}
	for i := 1; i+1 < len(launch.Args); i++ {
		if launch.Args[i] == "--data" && launch.Args[i+1] == root+"/data" {
			return nil
		}
	}
	return errors.New("supervised child must use the installation data directory")
}
