package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type clusterCheckSSHSession struct {
	ctx     context.Context
	node    clusterInstallNode
	scratch string
	command *exec.Cmd
	stdin   io.WriteCloser
	decoder *json.Decoder
	stderr  bytes.Buffer
	mutex   sync.Mutex
	closed  bool
}

func clusterCheckScratchCleanupCommand(scratch string) string {
	owner := strings.TrimPrefix(filepath.Base(scratch), "gpuworks-cluster-check-")
	return "if test -e " + clusterInstallQuote(scratch) + " || test -L " + clusterInstallQuote(scratch) + "; then set -eu; test ! -L " + clusterInstallQuote(scratch) + "; test \"$(cd " + clusterInstallQuote(scratch) + " && pwd -P)\" = " + clusterInstallQuote(scratch) + "; test -f " + clusterInstallQuote(scratch+"/owner") + "; test ! -L " + clusterInstallQuote(scratch+"/owner") + "; test \"$(cat " + clusterInstallQuote(scratch+"/owner") + ")\" = " + clusterInstallQuote(owner) + "; rm -f -- " + clusterInstallQuote(scratch+"/gpuworks") + " " + clusterInstallQuote(scratch+"/owner") + "; rmdir -- " + clusterInstallQuote(scratch) + "; fi"
}

func clusterCheckScratchCleanup(ctx context.Context, node clusterInstallNode, scratch string) error {
	// Never remove recursively: a changed scratch path or foreign file makes
	// cleanup fail visibly instead of deleting resources belonging to others.
	argv := append(clusterInstallSSHArgs(node), clusterCheckScratchCleanupCommand(scratch))
	raw, err := clusterInstallSSHCommand(ctx, argv).CombinedOutput()
	if err != nil {
		return fmt.Errorf("remove exact scratch %s: %s: %w", scratch, strings.TrimSpace(string(raw)), err)
	}
	return nil
}

func startClusterCheckSSH(ctx context.Context, node clusterInstallNode, binary []byte) (result clusterCheckSession, resultErr error) {
	unique := make([]byte, 16)
	if _, err := rand.Read(unique); err != nil {
		return nil, err
	}
	scratch := "/tmp/gpuworks-cluster-check-" + hex.EncodeToString(unique)
	defer func() {
		if resultErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, clusterCheckScratchCleanup(cleanupCtx, node, scratch))
		}
	}()
	digest := sha256.Sum256(binary)
	copyCommand := "set -eu; umask 077; mkdir -m 700 " + clusterInstallQuote(scratch) + "; trap " + clusterInstallQuote("rmdir -- "+clusterInstallQuote(scratch)+" 2>/dev/null || true") + " EXIT; set -C; printf '%s' " + clusterInstallQuote(hex.EncodeToString(unique)) + " > " + clusterInstallQuote(scratch+"/owner") + "; cat > " + clusterInstallQuote(scratch+"/gpuworks") + "; chmod 700 " + clusterInstallQuote(scratch+"/gpuworks") + "; echo " + clusterInstallQuote(hex.EncodeToString(digest[:])+"  "+scratch+"/gpuworks") + " | sha256sum -c - >/dev/null"
	argv := append(clusterInstallSSHArgs(node), copyCommand)
	copyProcess := clusterInstallSSHCommand(ctx, argv)
	copyProcess.Stdin = bytes.NewReader(binary)
	raw, err := copyProcess.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("copy check executable (verify SSH host key/account/access): %s: %w", strings.TrimSpace(string(raw)), err)
	}
	session := &clusterCheckSSHSession{ctx: ctx, node: node, scratch: scratch}
	// The helper owns no detached process. EOF on this SSH channel stops probes;
	// the shell trap removes only the copied executable and its empty directory.
	cleanup := clusterCheckScratchCleanupCommand(scratch)
	command := "trap " + clusterInstallQuote(cleanup) + " EXIT HUP INT TERM; " + clusterInstallQuote(scratch+"/gpuworks") + " cluster-check-node"
	session.command = clusterInstallSSHCommand(ctx, append(clusterInstallSSHArgs(node), command))
	session.command.Stderr = &session.stderr
	session.stdin, err = session.command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := session.command.StdoutPipe()
	if err != nil {
		session.stdin.Close()
		return nil, err
	}
	session.decoder = json.NewDecoder(stdout)
	session.decoder.DisallowUnknownFields()
	if err := session.command.Start(); err != nil {
		session.stdin.Close()
		stdout.Close()
		return nil, err
	}
	return session, nil
}

func (s *clusterCheckSSHSession) Exchange(req clusterCheckRequest) (clusterCheckResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	response := clusterCheckResponse{}
	if s.closed {
		return response, errors.New("check helper is closed")
	}
	if err := s.ctx.Err(); err != nil {
		return response, err
	}
	if err := json.NewEncoder(s.stdin).Encode(req); err != nil {
		return response, fmt.Errorf("write check helper command: %w", err)
	}
	if err := s.decoder.Decode(&response); err != nil {
		return response, fmt.Errorf("read check helper reply (check executable architecture/native support): %w", err)
	}
	return response, nil
}

func (s *clusterCheckSSHSession) Close() error {
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return nil
	}
	s.closed = true
	s.stdin.Close()
	s.mutex.Unlock()
	// CommandContext is canceled by the caller's deadline. A helper that does
	// not obey EOF cannot hold preflight open forever even on a live context.
	completed := make(chan error, 1)
	go func() { completed <- s.command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-completed:
	case <-time.After(10 * time.Second):
		_ = s.command.Process.Kill()
		waitErr = errors.Join(errors.New("check helper did not exit after EOF"), <-completed)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cleanupErr := clusterCheckScratchCleanup(cleanupCtx, s.node, s.scratch)
	if waitErr != nil {
		waitErr = fmt.Errorf("check helper exit: %s: %w", strings.TrimSpace(s.stderr.String()), waitErr)
	}
	return errors.Join(waitErr, cleanupErr)
}
