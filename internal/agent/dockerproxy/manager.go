// Package dockerproxy observes and configures the proxy of the Docker daemon.
//
// Docker pulls in the daemon, not in the docker CLI the agent runs, so the
// proxy a node needs for registries has to live in the daemon's environment.
// The agent writes it as a systemd drop-in and reloads systemd, but never
// restarts the daemon by itself: a restart stops every container unless
// live-restore is on, so it happens only when the operator asks (the
// runtime.restart RPC).
package dockerproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Paths, relative to Manager.Root.
const (
	dropInDir = "etc/systemd/system/docker.service.d"
	// dropInName sorts after any drop-in an operator wrote by hand
	// (proxy.conf, http-proxy.conf): systemd applies drop-ins in name order
	// and a later Environment= wins, so the proxy set in the console is the
	// one dockerd ends up with.
	dropInName      = "zz-gpuworks-proxy.conf"
	daemonJSON      = "etc/docker/daemon.json"
	systemdRunDir   = "run/systemd/system"
	observationTTL  = 30 * time.Second
	infoTimeout     = 10 * time.Second
	commandTimeout  = 30 * time.Second
	restartTimeout  = 2 * time.Minute
	restartPatience = 60 * time.Second
)

// Runner runs a command and returns its standard output. stdin may be nil.
// The error of a failed command carries the tail of its standard error.
type Runner interface {
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[len(msg)-200:]
		}
		if msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

// Manager owns the Docker daemon's proxy on this node. Everything it touches
// is reached through Root and Run, so tests run against a temp directory and a
// scripted command runner.
type Manager struct {
	// Root is the filesystem root ("/" in production).
	Root string
	// Run executes commands.
	Run Runner
	// Euid reports the effective user id.
	Euid func() int
	// Now, Sleep and RestartWait are the clock a restart waits on.
	Now         func() time.Time
	Sleep       func(time.Duration)
	RestartWait time.Duration
	Log         *slog.Logger

	mu       sync.Mutex
	cached   *proto.RuntimeProxyStatus
	cachedAt time.Time
	// infoOK: the cached observation got an answer from `docker info`.
	infoOK bool
	// applyErr is why the last Apply could not do what the config asked.
	applyErr string
}

// New returns the Manager for this host.
func New(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{Root: "/", Run: execRunner{}, Euid: os.Geteuid, Now: time.Now, Sleep: time.Sleep, RestartWait: restartPatience, Log: log}
}

func (m *Manager) path(rel string) string { return filepath.Join(m.Root, rel) }

func (m *Manager) dropInPath() string { return m.path(filepath.Join(dropInDir, dropInName)) }

// Status is what the daemon and the host say about the daemon's proxy. It is
// cached briefly, because it is read on every status report and costs a
// `docker info`.
func (m *Manager) Status(ctx context.Context) *proto.RuntimeProxyStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked(ctx, false)
}

// Daemon is the proxy the daemon reports using. ok is false when the daemon
// could not be asked.
func (m *Manager) Daemon(ctx context.Context) (httpProxy, httpsProxy, noProxy string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.statusLocked(ctx, false)
	if !m.infoOK {
		return "", "", "", false
	}
	return st.HTTPProxy, st.HTTPSProxy, st.NoProxy, true
}

func (m *Manager) statusLocked(ctx context.Context, fresh bool) *proto.RuntimeProxyStatus {
	if fresh || m.cached == nil || m.Now().Sub(m.cachedAt) >= observationTTL {
		m.cached = m.observe(ctx)
		m.cachedAt = m.Now()
	}
	out := *m.cached
	if m.applyErr != "" {
		if out.Error != "" {
			out.Error += "; "
		}
		out.Error += m.applyErr
	}
	return &out
}

func (m *Manager) observe(ctx context.Context) *proto.RuntimeProxyStatus {
	st := &proto.RuntimeProxyStatus{}
	info, err := m.dockerInfo(ctx)
	m.infoOK = err == nil
	if err != nil {
		st.Error = err.Error()
	} else {
		st.HTTPProxy, st.HTTPSProxy, st.NoProxy, st.LiveRestore = info.HTTPProxy, info.HTTPSProxy, info.NoProxy, info.LiveRestore
	}
	st.DaemonJSONProxy = m.daemonJSONHasProxies()
	st.Systemd = m.systemdManagesDocker(ctx)
	if st.Systemd {
		st.ConfiguredHTTPProxy, st.ConfiguredHTTPSProxy, st.ConfiguredKnown = m.configuredProxy(ctx)
	}
	st.Privilege = m.privilege(ctx)
	if raw, err := os.ReadFile(m.dropInPath()); err == nil {
		// The drop-in holds the proxy's password in clear; the report goes
		// to the console and is masked like the daemon's own.
		w := parseDropIn(string(raw))
		st.WrittenHTTPProxy, st.WrittenHTTPSProxy, st.WrittenNoProxy = proto.MaskProxyCredentials(w.HTTP), proto.MaskProxyCredentials(w.HTTPS), w.No
	}
	return st
}

type daemonInfo struct {
	HTTPProxy, HTTPSProxy, NoProxy string
	LiveRestore                    bool
}

func (m *Manager) dockerInfo(ctx context.Context) (daemonInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()
	out, err := m.Run.Run(ctx, nil, "docker", "info", "--format", "{{json .}}")
	if err != nil {
		return daemonInfo{}, fmt.Errorf("docker info failed: %w", err)
	}
	var raw struct {
		HTTPProxy   *string `json:"HttpProxy"`
		HTTPSProxy  *string `json:"HttpsProxy"`
		NoProxy     *string `json:"NoProxy"`
		LiveRestore bool    `json:"LiveRestoreEnabled"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return daemonInfo{}, errors.New("docker info returned unparsable output")
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	return daemonInfo{HTTPProxy: str(raw.HTTPProxy), HTTPSProxy: str(raw.HTTPSProxy), NoProxy: str(raw.NoProxy), LiveRestore: raw.LiveRestore}, nil
}

// configuredProxy is the proxy the daemon starts with: the docker unit's
// environment as systemd has it loaded, every drop-in included (after
// daemon-reload). A lower-case name counts when the upper-case one is unset,
// as it does for the daemon. Credentials are masked as docker info masks them.
func (m *Manager) configuredProxy(ctx context.Context) (httpProxy, httpsProxy string, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := m.Run.Run(ctx, nil, "systemctl", "show", "docker.service", "--property=Environment", "--value")
	if err != nil {
		return "", "", false
	}
	env := map[string]string{}
	for _, word := range splitQuoted(strings.TrimSpace(string(out))) {
		if key, value, found := strings.Cut(word, "="); found {
			env[key] = value
		}
	}
	pick := func(upper, lower string) string {
		if value := env[upper]; value != "" {
			return value
		}
		return env[lower]
	}
	return proto.MaskProxyCredentials(pick("HTTP_PROXY", "http_proxy")), proto.MaskProxyCredentials(pick("HTTPS_PROXY", "https_proxy")), true
}

func (m *Manager) daemonJSONHasProxies() bool {
	raw, err := os.ReadFile(m.path(daemonJSON))
	if err != nil {
		return false
	}
	var cfg struct {
		Proxies map[string]json.RawMessage `json:"proxies"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false
	}
	return len(cfg.Proxies) > 0
}

func (m *Manager) systemdManagesDocker(ctx context.Context) bool {
	if info, err := os.Stat(m.path(systemdRunDir)); err != nil || !info.IsDir() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	_, err := m.Run.Run(ctx, nil, "systemctl", "cat", "docker.service")
	return err == nil
}

func (m *Manager) privilege(ctx context.Context) string {
	if m.Euid() == 0 {
		return proto.RuntimePrivilegeRoot
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if _, err := m.Run.Run(ctx, nil, "sudo", "-n", "true"); err == nil {
		return proto.RuntimePrivilegeSudo
	}
	return proto.RuntimePrivilegeNone
}

// privileged runs a command as root: directly when the agent is root, else
// through passwordless sudo.
func (m *Manager) privileged(ctx context.Context, privilege string, stdin []byte, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if privilege == proto.RuntimePrivilegeSudo {
		args = append([]string{"-n", name}, args...)
		name = "sudo"
	}
	_, err := m.Run.Run(ctx, stdin, name, args...)
	return err
}

// Apply makes the drop-in match the network config: the configured proxy (and
// bypass list) when there is one, no drop-in when there is none. It reloads
// systemd so the next daemon start picks the change up, and never restarts the
// daemon. A node whose daemon the agent cannot configure is left alone and
// says why in Status; Apply's error is only for the caller's log.
func (m *Manager) Apply(ctx context.Context, cfg proto.NetworkConfig, serverURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	desired := desiredProxy(cfg, serverURL)
	var current proxyEnv
	raw, readErr := os.ReadFile(m.dropInPath())
	exists := readErr == nil
	if exists {
		current = parseDropIn(string(raw))
	}
	if (desired.empty() && !exists) || (exists && current == desired) {
		m.applyErr = ""
		return nil
	}

	st := m.statusLocked(ctx, true)
	err := m.applyLocked(ctx, st, desired)
	if err != nil {
		m.applyErr = err.Error()
		m.Log.Warn("docker daemon proxy not applied", "err", err)
		return err
	}
	m.applyErr = ""
	m.cached = nil
	if desired.empty() {
		m.Log.Info("docker daemon proxy drop-in removed; restart the daemon to stop using the old proxy")
	} else {
		m.Log.Info("docker daemon proxy drop-in written; restart the daemon to use it", "proxy", redact(desired.HTTPS))
	}
	return nil
}

func (m *Manager) applyLocked(ctx context.Context, st *proto.RuntimeProxyStatus, desired proxyEnv) error {
	if !st.Systemd {
		return errors.New("the Docker daemon is not a systemd unit the agent can configure")
	}
	if st.Privilege == proto.RuntimePrivilegeNone {
		return errors.New("the agent has neither root nor passwordless sudo, so it cannot configure the Docker daemon's proxy")
	}
	path := m.dropInPath()
	if desired.empty() {
		if st.Privilege == proto.RuntimePrivilegeRoot {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove docker proxy drop-in: %w", err)
			}
		} else if err := m.privileged(ctx, st.Privilege, nil, "rm", "-f", path); err != nil {
			return fmt.Errorf("remove docker proxy drop-in: %w", err)
		}
	} else {
		content, err := renderDropIn(desired)
		if err != nil {
			return fmt.Errorf("docker proxy drop-in: %w", err)
		}
		if st.Privilege == proto.RuntimePrivilegeRoot {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("write docker proxy drop-in: %w", err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return fmt.Errorf("write docker proxy drop-in: %w", err)
			}
		} else {
			if err := m.privileged(ctx, st.Privilege, nil, "mkdir", "-p", filepath.Dir(path)); err != nil {
				return fmt.Errorf("write docker proxy drop-in: %w", err)
			}
			if err := m.privileged(ctx, st.Privilege, []byte(content), "tee", path); err != nil {
				return fmt.Errorf("write docker proxy drop-in: %w", err)
			}
		}
	}
	if err := m.privileged(ctx, st.Privilege, nil, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
}

// Restart restarts the Docker daemon so the drop-in takes effect, waits for it
// to answer again, and returns what it now reports. It refuses, saying why,
// where the agent cannot restart the daemon safely.
func (m *Manager) Restart(ctx context.Context) (*proto.RuntimeProxyStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.statusLocked(ctx, true)
	if !st.Systemd {
		return nil, errors.New("the Docker daemon is not a systemd unit; restart it on the node")
	}
	if st.Privilege == proto.RuntimePrivilegeNone {
		return nil, errors.New("the agent has neither root nor passwordless sudo, so it cannot restart the Docker daemon")
	}
	rctx, cancel := context.WithTimeout(ctx, restartTimeout)
	defer cancel()
	m.Log.Info("restarting the docker daemon", "live_restore", st.LiveRestore)
	if err := m.privileged(rctx, st.Privilege, nil, "systemctl", "restart", "docker"); err != nil {
		m.cached = nil
		return nil, fmt.Errorf("systemctl restart docker: %w", err)
	}
	deadline := m.Now().Add(m.RestartWait)
	for {
		if _, err := m.dockerInfo(rctx); err == nil {
			break
		}
		if rctx.Err() != nil {
			m.cached = nil
			return nil, fmt.Errorf("waiting for the Docker daemon after restart: %w", rctx.Err())
		}
		if !m.Now().Before(deadline) {
			m.cached = nil
			return nil, fmt.Errorf("the Docker daemon did not answer within %s after restart", m.RestartWait)
		}
		m.Sleep(time.Second)
	}
	return m.statusLocked(ctx, true), nil
}
