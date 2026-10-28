// Package experiencelaunch starts and stops the experience environment
// beside a server: a separate `gpuworks experience` process with its own
// data directory and console port (internal/experience), so showing the
// product never touches the platform's own nodes, models or data.
package experiencelaunch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// User is the environment console's administrator.
const User = "admin"

// Config locates the environment.
type Config struct {
	// Binary is the gpuworks executable that runs `experience`.
	Binary string
	// DataDir is the environment's own directory; nothing outside it is
	// written.
	DataDir string
	// Listen are the environment console's addresses, the server's console
	// addresses on Port; Addr replaces them for a server started with one
	// address for everything (--addr).
	Listen []string
	Addr   string
	Port   int
	// TLSCertFile and TLSKeyFile are the server's own certificate, which the
	// environment's public listeners use too.
	TLSCertFile, TLSKeyFile string
}

// Launcher owns at most one environment process. ops serializes the
// actions that start or stop it; mu guards the state Status reads.
type Launcher struct {
	cfg Config
	log *slog.Logger
	ops sync.Mutex

	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{}
	password string
	lastErr  string
}

func New(cfg Config, log *slog.Logger) *Launcher { return &Launcher{cfg: cfg, log: log} }

func (l *Launcher) dataDir() string      { return filepath.Join(l.cfg.DataDir, "data") }
func (l *Launcher) passwordFile() string { return filepath.Join(l.cfg.DataDir, "console-password") }

// SetEnabled starts the environment from its initial data with a new
// password, or stops it. Starting a running environment changes nothing.
func (l *Launcher) SetEnabled(_ context.Context, on bool) error {
	l.ops.Lock()
	defer l.ops.Unlock()
	if !on {
		l.stop()
		return nil
	}
	if l.running() {
		return nil
	}
	if _, err := l.newPassword(); err != nil {
		return err
	}
	return l.start()
}

// Reset starts the environment again from its initial data; its password
// stays, so a visitor already given the account keeps it.
func (l *Launcher) Reset(context.Context) error {
	l.ops.Lock()
	defer l.ops.Unlock()
	if !l.running() {
		return errors.New("the experience environment is not running")
	}
	l.stop()
	return l.start()
}

// RotatePassword gives the running environment a new console password. The
// environment follows the password file and ends the old password's
// sessions.
func (l *Launcher) RotatePassword(context.Context) error {
	l.ops.Lock()
	defer l.ops.Unlock()
	if !l.running() {
		return errors.New("the experience environment is not running")
	}
	_, err := l.newPassword()
	return err
}

func (l *Launcher) running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cmd != nil
}

// start runs the environment on fresh data. ops is held.
func (l *Launcher) start() error {
	if err := os.RemoveAll(l.dataDir()); err != nil {
		return fmt.Errorf("clear the previous data: %w", err)
	}
	if err := os.MkdirAll(l.cfg.DataDir, 0o700); err != nil {
		return err
	}
	if err := l.portFree(); err != nil {
		return err
	}
	raw, err := os.ReadFile(l.passwordFile())
	if err != nil {
		return err
	}
	logPath := filepath.Join(l.cfg.DataDir, "experience.log")
	_ = os.Remove(logPath)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	args := []string{"experience", "--data", l.dataDir(), "--password-file", l.passwordFile(), "--parent-pid", strconv.Itoa(os.Getpid())}
	if l.cfg.Addr != "" {
		args = append(args, "--addr", l.cfg.Addr)
	} else {
		args = append(args, "--listen", strings.Join(l.cfg.Listen, ","))
	}
	if l.cfg.TLSCertFile != "" {
		args = append(args, "--tls-cert", l.cfg.TLSCertFile, "--tls-key", l.cfg.TLSKeyFile)
	}
	cmd := exec.Command(l.cfg.Binary, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start the experience environment: %w", err)
	}
	exited := make(chan struct{})
	l.mu.Lock()
	l.cmd, l.exited, l.password, l.lastErr = cmd, exited, strings.TrimSpace(string(raw)), ""
	l.mu.Unlock()
	go func() {
		err := cmd.Wait()
		logFile.Close()
		l.mu.Lock()
		if l.cmd == cmd {
			l.cmd = nil
			if err != nil {
				l.lastErr = lastError(logPath, err)
			}
		}
		l.mu.Unlock()
		close(exited)
		l.log.Info("experience environment exited", "err", err)
	}()
	l.log.Info("experience environment started", "port", l.cfg.Port, "pid", cmd.Process.Pid)
	return nil
}

// portFree fails early, with the port in the message, when another program
// holds the environment's port.
func (l *Launcher) portFree() error {
	addrs := l.cfg.Listen
	if l.cfg.Addr != "" {
		addrs = []string{l.cfg.Addr}
	}
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return messages.WrapProductError(messages.ErrorExperiencePortInUse,
				fmt.Sprintf("port %d is held by another program; free it, or move the server's console port so the environment takes another", l.cfg.Port),
				map[string]any{"port": l.cfg.Port}, err)
		}
		_ = ln.Close()
	}
	return nil
}

// newPassword writes a fresh random console password.
func (l *Launcher) newPassword() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	password := hex.EncodeToString(buf)
	if err := os.MkdirAll(l.cfg.DataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(l.passwordFile(), []byte(password), 0o600); err != nil {
		return "", err
	}
	l.mu.Lock()
	l.password = password
	l.mu.Unlock()
	return password, nil
}

// stop ends the environment and waits for it. ops is held.
func (l *Launcher) stop() {
	l.mu.Lock()
	cmd, exited := l.cmd, l.exited
	l.cmd = nil
	l.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
}

// Status is what the settings card shows.
func (l *Launcher) Status(ctx context.Context) dto.ExperienceStatus {
	l.mu.Lock()
	st := dto.ExperienceStatus{Enabled: l.cmd != nil, Port: l.cfg.Port, User: User, Error: l.lastErr}
	if st.Enabled {
		st.Password = l.password
	}
	l.mu.Unlock()
	if st.Enabled {
		st.Ready = l.answers(ctx)
	}
	return st
}

// answers reports whether the environment's console is up, asked on the
// loopback address every listener set includes.
func (l *Launcher) answers(ctx context.Context) bool {
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(l.cfg.Port))
	if l.cfg.Addr != "" {
		if host, port, err := net.SplitHostPort(l.cfg.Addr); err == nil {
			if ip := net.ParseIP(host); host != "" && ip != nil && !ip.IsUnspecified() {
				target = net.JoinHostPort(host, port)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// lastError is why the environment stopped: the error it logged last, which
// names the cause, or else how its process ended.
func lastError(logPath string, exit error) string {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return exit.Error()
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-50; i-- {
		if !strings.Contains(lines[i], "level=ERROR") {
			continue
		}
		if _, msg, ok := strings.Cut(lines[i], " err="); ok {
			return strings.Trim(msg, `"`)
		}
		return lines[i]
	}
	return exit.Error()
}
