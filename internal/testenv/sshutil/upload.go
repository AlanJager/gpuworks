package sshutil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/commandlog"
)

// UploadFile atomically uploads a local file over SSH. The destination is
// written through a sibling temporary file so a canceled test never leaves a
// partially replaced server or agent binary at the final path.
func UploadFile(ctx context.Context, host string, port int, username string, privateKey []byte, localPath, remotePath string, mode os.FileMode) error {
	return uploadLocalFile(ctx, host, port, username, []ssh.AuthMethod{publicKeyAuth(privateKey)}, localPath, remotePath, mode)
}

// UploadFilePassword is UploadFile using SSH password authentication.
func UploadFilePassword(ctx context.Context, host string, port int, username, password, localPath, remotePath string, mode os.FileMode) error {
	return uploadLocalFile(ctx, host, port, username, []ssh.AuthMethod{ssh.Password(password)}, localPath, remotePath, mode)
}

// UploadBytes atomically uploads an in-memory file over SSH.
func UploadBytes(ctx context.Context, host string, port int, username string, privateKey []byte, data []byte, remotePath string, mode os.FileMode) error {
	return uploadReader(ctx, host, port, username, []ssh.AuthMethod{publicKeyAuth(privateKey)}, bytes.NewReader(data), fingerprintBytes(data, mode), remotePath, mode)
}

// UploadBytesPassword is UploadBytes using SSH password authentication.
func UploadBytesPassword(ctx context.Context, host string, port int, username, password string, data []byte, remotePath string, mode os.FileMode) error {
	return uploadReader(ctx, host, port, username, []ssh.AuthMethod{ssh.Password(password)}, bytes.NewReader(data), fingerprintBytes(data, mode), remotePath, mode)
}

func uploadLocalFile(ctx context.Context, host string, port int, username string, auth []ssh.AuthMethod, localPath, remotePath string, mode os.FileMode) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open upload source: %w", err)
	}
	defer file.Close()
	fingerprint, err := fingerprintFile(file, mode)
	if err != nil {
		return fmt.Errorf("fingerprint upload source: %w", err)
	}
	return uploadReader(ctx, host, port, username, auth, file, fingerprint, remotePath, mode)
}

type uploadFingerprint struct {
	size   int64
	sha256 string
	mode   os.FileMode
}

func fingerprintBytes(data []byte, mode os.FileMode) uploadFingerprint {
	sum := sha256.Sum256(data)
	return uploadFingerprint{size: int64(len(data)), sha256: fmt.Sprintf("%x", sum), mode: uploadMode(mode)}
}

func fingerprintFile(file *os.File, mode os.FileMode) (uploadFingerprint, error) {
	info, err := file.Stat()
	if err != nil {
		return uploadFingerprint{}, err
	}
	if !info.Mode().IsRegular() {
		return uploadFingerprint{}, fmt.Errorf("source is not a regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return uploadFingerprint{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return uploadFingerprint{}, err
	}
	return uploadFingerprint{size: info.Size(), sha256: fmt.Sprintf("%x", hash.Sum(nil)), mode: uploadMode(mode)}, nil
}

func uploadMode(mode os.FileMode) os.FileMode {
	if mode.Perm() == 0 {
		return 0o600
	}
	return mode.Perm()
}

func uploadReader(ctx context.Context, host string, port int, username string, auth []ssh.AuthMethod, reader io.Reader, fingerprint uploadFingerprint, remotePath string, mode os.FileMode) error {
	command, err := atomicUploadCommand(remotePath, mode, time.Now().UnixNano())
	if err != nil {
		return err
	}
	client, err := dialAuth(ctx, host, port, username, auth)
	if err != nil {
		return err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	session.Stdin = reader
	output := captureSessionOutput(session)
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	go maintainSSHConnection(keepaliveDone, client, sshKeepaliveInterval)
	execution := commandlog.Start(command)
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err := <-done:
		execution.Finish(output.String(), err)
		if err != nil {
			// Some cloud SSH gateways close an otherwise completed exec channel
			// without forwarding its exit-status. Never accept that ambiguous
			// outcome on faith: reconnect and prove the atomic destination has the
			// exact content and permissions requested by the caller.
			if verifyErr := verifyRemoteUpload(ctx, host, port, username, auth, remotePath, fingerprint); verifyErr == nil {
				return nil
			} else {
				return fmt.Errorf("upload remote file: %w: %s (postcondition: %v)", err, strings.TrimSpace(output.String()), verifyErr)
			}
		}
		return nil
	case <-ctx.Done():
		_ = client.Close()
		<-done
		execution.Finish(output.String(), ctx.Err())
		return fmt.Errorf("upload remote file canceled: %w", ctx.Err())
	}
}

func verifyRemoteUpload(ctx context.Context, host string, port int, username string, auth []ssh.AuthMethod, remotePath string, want uploadFingerprint) error {
	command, err := uploadVerificationCommand(remotePath)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		client, err := dialAuth(ctx, host, port, username, auth)
		if err == nil {
			verifyCtx, cancel := withMaxTimeout(ctx, 2*time.Minute)
			var output string
			output, err = runContext(verifyCtx, client, command)
			cancel()
			_ = client.Close()
			if err == nil {
				if err := checkUploadFingerprint(output, want); err == nil {
					return nil
				} else {
					lastErr = err
				}
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return lastErr
}

func uploadVerificationCommand(remotePath string) (string, error) {
	remotePath = strings.TrimSpace(remotePath)
	if remotePath == "" || !path.IsAbs(remotePath) || path.Clean(remotePath) == "/" {
		return "", fmt.Errorf("remote upload path must be an absolute file path")
	}
	clean := path.Clean(remotePath)
	return fmt.Sprintf("test -f %s && printf '%%s %%s %%s\\n' \"$(wc -c < %s | tr -d '[:space:]')\" \"$(sha256sum %s | cut -d ' ' -f 1)\" \"$(stat -c %%a %s)\"",
		shellQuote(clean), shellQuote(clean), shellQuote(clean), shellQuote(clean)), nil
}

func checkUploadFingerprint(output string, want uploadFingerprint) error {
	fields := strings.Fields(output)
	if len(fields) != 3 {
		return fmt.Errorf("remote fingerprint output %q has %d fields, want 3", strings.TrimSpace(output), len(fields))
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return fmt.Errorf("parse remote byte count %q: %w", fields[0], err)
	}
	wantMode := strconv.FormatUint(uint64(uploadMode(want.mode).Perm()), 8)
	if size != want.size || fields[1] != want.sha256 || fields[2] != wantMode {
		return fmt.Errorf("remote fingerprint size/hash/mode=%d/%s/%s, want %d/%s/%s", size, fields[1], fields[2], want.size, want.sha256, wantMode)
	}
	return nil
}

func atomicUploadCommand(remotePath string, mode os.FileMode, nonce int64) (string, error) {
	remotePath = strings.TrimSpace(remotePath)
	if remotePath == "" || !path.IsAbs(remotePath) || path.Clean(remotePath) == "/" {
		return "", fmt.Errorf("remote upload path must be an absolute file path")
	}
	clean := path.Clean(remotePath)
	dir := path.Dir(clean)
	temporary := clean + ".upload-" + strconv.FormatInt(nonce, 10)
	permissions := uploadMode(mode)
	return fmt.Sprintf("umask 077 && mkdir -p %s && cat > %s && chmod %04o %s && mv -f %s %s",
		shellQuote(dir), shellQuote(temporary), permissions, shellQuote(temporary), shellQuote(temporary), shellQuote(clean)), nil
}
