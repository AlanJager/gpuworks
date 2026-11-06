package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/version"
)

// serverSettings serves api.ServerSettings from the running configuration and
// server.yaml. Saved listen changes take effect on the next start.
type serverSettings struct {
	cfg              Config
	host             serverconfig.Host
	readPrivateHosts func() []string
	// restart ends the process for the service manager to start again; nil
	// when nothing would start it (development, the nohup service mode).
	restart func()
	// snapshot writes a consistent copy of the live database, for the backup
	// a console upgrade takes; nil when there is no database.
	snapshot func(dest string) error
	// executablePath replaces os.Executable in tests.
	executablePath string

	mu    sync.Mutex
	saved serverconfig.Listen
	// channel is update_channel as last read or saved; empty follows the build.
	channel string
	// manualChecks is auto_check_updates: false as last read or saved.
	manualChecks bool
	// usageReportOff is usage_report: false as last read or saved.
	usageReportOff bool
	// installID is set once at assembly (see installIDOf) and never changes.
	installID string
}

func newServerSettings(cfg Config, restart func(), snapshot func(dest string) error) *serverSettings {
	host := cfg.Host
	if host.RouteIP == nil && host.Interfaces == nil {
		host = serverconfig.SystemHost()
	}
	return &serverSettings{cfg: cfg, host: host, readPrivateHosts: privateHostPermissions(cfg, nil), restart: restart, snapshot: snapshot, saved: cfg.Listen, channel: cfg.UpdateChannel, manualChecks: cfg.ManualUpdateChecks, usageReportOff: cfg.UsageReportOff}
}

var _ api.ServerSettings = (*serverSettings)(nil)

// serverSettings returns the app's settings, building them for an App
// assembled without New (handler unit tests).
func (a *App) serverSettings() *serverSettings {
	if a.settings == nil {
		a.settings = newServerSettings(a.cfg, nil, nil)
	}
	return a.settings
}

// LoopbackURL is the base URL this server answers on over loopback, for the
// requests it makes to itself. Every listening port also takes 127.0.0.1
// (serverconfig.Bindings), so the first console port is enough.
func (s *serverSettings) LoopbackURL() string {
	for _, b := range s.cfg.bindings() {
		if !b.Serves(serverconfig.FunctionConsole) {
			continue
		}
		_, port, err := net.SplitHostPort(b.Addr)
		if err != nil || port == "" {
			continue
		}
		return "http://" + net.JoinHostPort("127.0.0.1", port)
	}
	return ""
}

func (s *serverSettings) Listen() dto.ServerListen {
	s.mu.Lock()
	saved := s.saved
	s.mu.Unlock()
	out := dto.ServerListen{
		Console: nonNil(saved.Console), Agent: nonNil(saved.Agent), Gateway: nonNil(saved.Gateway),
		ConfigPath: s.cfg.ConfigPath,
		Editable:   s.cfg.ConfigPath != "" && !s.cfg.ListenOverridden,
		Overridden: s.cfg.ListenOverridden,
		Running:    []dto.ServerBinding{},
	}
	for _, b := range s.cfg.bindings() {
		out.Running = append(out.Running, dto.ServerBinding{Addr: b.Addr, Functions: b.Functions})
	}
	if out.Editable {
		out.RestartPending = !sameBindings(serverconfig.Bindings(saved), s.cfg.bindings())
	}
	out.RestartMode, out.RestartCommand = "command", s.restartCommand()
	if s.restart != nil {
		out.RestartMode, out.RestartCommand = "self", ""
	}
	return out
}

// UpdateChannel is the channel update checks and upgrades read, and whether
// the console may change it (only with a server.yaml to keep it in).
func (s *serverSettings) UpdateChannel() (string, bool) {
	s.mu.Lock()
	channel := s.channel
	s.mu.Unlock()
	if channel == "" {
		channel = version.Channel
	}
	return channel, s.cfg.ConfigPath != ""
}

// SetUpdateChannel saves update_channel in server.yaml and applies it at once:
// unlike the listen addresses, nothing has to restart to read it.
func (s *serverSettings) SetUpdateChannel(channel string) error {
	if s.cfg.ConfigPath == "" {
		return &api.ServerSettingsError{Code: api.ErrorUpdateChannelReadOnly,
			Fallback: "the server was started without server.yaml, so its update channel cannot be changed here"}
	}
	if !distribution.KnownChannel(channel) {
		return &api.ServerSettingsError{Code: api.ErrorUpdateChannelInvalid,
			Fallback: "unknown update channel " + channel, Params: map[string]any{"channel": channel}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := serverconfig.SetUpdateChannel(s.cfg.ConfigPath, channel); err != nil {
		return err
	}
	s.channel = channel
	return nil
}

// AutoCheckUpdates is whether the server checks the channel by itself, and
// whether the console may change it (only with a server.yaml to keep it in).
func (s *serverSettings) AutoCheckUpdates() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.manualChecks, s.cfg.ConfigPath != ""
}

// SetAutoCheckUpdates saves auto_check_updates in server.yaml and applies it
// at once.
func (s *serverSettings) SetAutoCheckUpdates(on bool) error {
	if s.cfg.ConfigPath == "" {
		return &api.ServerSettingsError{Code: api.ErrorUpdateAutoCheckReadOnly,
			Fallback: "the server was started without server.yaml, so automatic update checks cannot be changed here"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := serverconfig.SetAutoCheckUpdates(s.cfg.ConfigPath, on); err != nil {
		return err
	}
	s.manualChecks = !on
	return nil
}

// UsageReport is whether the installation takes part in the user experience
// improvement program, and whether the console may change it. Only an
// installation (a server started with server.yaml) takes part: a server a
// test or a developer starts without one never reports.
func (s *serverSettings) UsageReport() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	installed := s.cfg.ConfigPath != ""
	return installed && !s.usageReportOff, installed
}

// SetUsageReport saves usage_report in server.yaml and applies it at once.
func (s *serverSettings) SetUsageReport(on bool) error {
	if s.cfg.ConfigPath == "" {
		return &api.ServerSettingsError{Code: api.ErrorUsageReportReadOnly,
			Fallback: "the server was started without server.yaml, so the usage report cannot be changed here"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := serverconfig.SetUsageReport(s.cfg.ConfigPath, on); err != nil {
		return err
	}
	s.usageReportOff = !on
	return nil
}

// InstallID names this installation to the update channel.
func (s *serverSettings) InstallID() string { return s.installID }

// installIDOf derives the installation's name from its master secret with a
// keyed hash, so the channel can tell installations apart and the secret
// cannot be recovered from the name.
func installIDOf(master string) string {
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte("gpuworks update channel installation id"))
	return hex.EncodeToString(mac.Sum(nil)[:8])
}

func (s *serverSettings) restartCommand() string {
	if s.cfg.ConfigPath == "" {
		return "gpuworks restart"
	}
	return "gpuworks restart --dir " + filepath.Dir(s.cfg.ConfigPath)
}

func (s *serverSettings) UpdateListen(update dto.ServerListenUpdate, localAddr string) (dto.ServerListen, error) {
	if s.cfg.ConfigPath == "" {
		return dto.ServerListen{}, &api.ServerSettingsError{Code: api.ErrorServerListenReadOnly,
			Fallback: "the server was started without server.yaml, so its addresses cannot be changed here"}
	}
	if s.cfg.ListenOverridden {
		return dto.ServerListen{}, &api.ServerSettingsError{Code: api.ErrorServerListenReadOnly,
			Fallback: "--addr on the command line overrides server.yaml; remove it to manage addresses here"}
	}
	next := serverconfig.Listen{Console: update.Console, Agent: update.Agent, Gateway: update.Gateway}
	if err := next.Validate(); err != nil {
		return dto.ServerListen{}, &api.ServerSettingsError{Code: api.ErrorServerListenInvalid, Fallback: err.Error(), Params: map[string]any{"reason": err.Error()}}
	}
	if s.cfg.TLSCertFile != "" || s.cfg.TLSKeyFile != "" {
		candidate := s.cfg
		candidate.Listen = next
		if _, err := candidate.listenerTLS(); err != nil {
			return dto.ServerListen{}, &api.ServerSettingsError{Code: api.ErrorServerListenInvalid, Fallback: err.Error(), Params: map[string]any{"reason": err.Error()}}
		}
	}
	if len(next.Of(serverconfig.FunctionConsole)) == 0 {
		return dto.ServerListen{}, &api.ServerSettingsError{Code: api.ErrorServerListenInvalid, Fallback: "the console needs an address", Params: map[string]any{"reason": "listen.console is empty"}}
	}
	local, err := serverconfig.LocalIPs(s.host)
	if err != nil {
		return dto.ServerListen{}, err
	}
	for _, fn := range serverconfig.Functions {
		for _, addr := range next.Of(fn) {
			ip, _, _ := serverconfig.SplitAddr(addr)
			if !ip.IsUnspecified() && !ip.IsLoopback() && !local[ip.String()] {
				return dto.ServerListen{}, notLocal(addr)
			}
		}
	}
	if !keepsConsole(next, localAddr) {
		return dto.ServerListen{}, &api.ServerSettingsError{Code: api.ErrorServerListenLockout,
			Fallback: fmt.Sprintf("you are connected through %s, which would no longer serve the console after a restart", localAddr),
			Params:   map[string]any{"address": localAddr}}
	}
	s.mu.Lock()
	err = serverconfig.SetListen(s.cfg.ConfigPath, next)
	if err == nil {
		s.saved = next
	}
	s.mu.Unlock()
	if err != nil {
		return dto.ServerListen{}, err
	}
	return s.Listen(), nil
}

func notLocal(addr string) error {
	return &api.ServerSettingsError{Code: api.ErrorServerListenNotLocal,
		Fallback: fmt.Sprintf("%s is not an address of this machine", addr),
		Params:   map[string]any{"address": addr}}
}

// keepsConsole reports whether the address a request arrived on still serves
// the console under a new configuration. Loopback always does.
func keepsConsole(next serverconfig.Listen, localAddr string) bool {
	ip, port, err := serverconfig.SplitAddr(localAddr)
	if err != nil || serverconfig.ServesEverything(ip) {
		return true
	}
	for _, b := range serverconfig.Bindings(next) {
		if !b.Serves(serverconfig.FunctionConsole) {
			continue
		}
		bip, bport, err := serverconfig.SplitAddr(b.Addr)
		if err != nil || bport != port {
			continue
		}
		if bip.IsUnspecified() || bip.Equal(ip) {
			return true
		}
	}
	return false
}

func (s *serverSettings) Restart() error {
	if s.restart == nil {
		return &api.ServerSettingsError{Code: api.ErrorServerRestartManual,
			Fallback: "no service manager restarts this server; run " + s.restartCommand() + " on its host",
			Params:   map[string]any{"command": s.restartCommand()}}
	}
	s.restart()
	return nil
}

func sameBindings(a, b []serverconfig.Binding) bool {
	key := func(bs []serverconfig.Binding) string {
		parts := make([]string, 0, len(bs))
		for _, x := range bs {
			fns := append([]string(nil), x.Functions...)
			sort.Strings(fns)
			parts = append(parts, x.Addr+"="+strings.Join(fns, ","))
		}
		sort.Strings(parts)
		return strings.Join(parts, ";")
	}
	return key(a) == key(b)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// StorageProbeRoots is local policy, never editable through the management API.
func (s *serverSettings) StorageProbeRoots() []string {
	return append([]string(nil), s.cfg.StorageProbeRoots...)
}
