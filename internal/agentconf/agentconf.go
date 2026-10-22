// Package agentconf is the agent's launch configuration and the one place it
// is defined: what the agent connects with (server, token, connection
// direction, listen address) and how it runs workloads (runtime driver, mount
// root, udocker bundle). An install writes it once to <state>/FileName
// (gpuworks-agent configure); every way of starting the agent — the systemd
// unit, nohup and its restart script, the launchd job, a restart after an
// upgrade — runs only `gpuworks-agent run --state DIR`, and whatever looks for
// an agent on a host reads the file in the state directory its command line
// names.
//
// It is a leaf with no imports from this repository: the agent reads and
// writes the file, the server validates the same fields before it hands them
// to an installer, and the install script reads it through ShellFuncs.
package agentconf

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileName is the configuration file inside the agent's state directory.
const FileName = "gpuworks-agent.conf"

// Connection directions.
const (
	// AgentDial: the agent dials the server's /api/v1/agent/connect.
	AgentDial = "agent_dial"
	// ServerDial: the server dials the agent's listen address; the agent never
	// contacts the server, so it needs no server address.
	ServerDial = "server_dial"
)

// DefaultListen is where a server_dial agent listens when nothing else says.
const DefaultListen = "0.0.0.0:18800"

// Config is the agent's launch configuration.
type Config struct {
	// Server is the server's base URL (http or https). Required to dial the
	// server; a server_dial agent has none.
	Server string
	// Token is the per-node token, the agent's only credential.
	Token string
	// ConnectMode is AgentDial or ServerDial; empty means AgentDial.
	ConnectMode string
	// Listen is the server_dial listen address, host:port or :port.
	Listen string
	// RuntimeDriver forces a runtime driver; empty detects one.
	RuntimeDriver string
	// MountRoot is where a non-root runtime keeps bind-mount sources.
	MountRoot string
	// UdockerBundle is the directory of an offline udocker bundle.
	UdockerBundle string
}

// Errors Validate reports; callers that map them to their own codes (the
// API's SERVER_URL_REQUIRED) test with errors.Is.
var (
	ErrServerRequired = errors.New("the server URL is required when the agent dials the server")
	ErrServerInvalid  = errors.New("the server URL is invalid")
	ErrTokenRequired  = errors.New("the node token is required")
	ErrModeInvalid    = errors.New("the connection direction must be agent_dial or server_dial")
	ErrListenInvalid  = errors.New("the listen address must be host:port or :port with a port from 1 to 65535")
	ErrValueInvalid   = errors.New("a value contains a line break or control character")
)

// Mode is the connection direction with the default applied.
func (c Config) Mode() string {
	if strings.TrimSpace(c.ConnectMode) == ServerDial {
		return ServerDial
	}
	return AgentDial
}

// ServerDials reports whether the server dials this agent.
func (c Config) ServerDials() bool { return c.Mode() == ServerDial }

// Normalized trims every field, drops a trailing slash from Server, writes
// the connection direction out, and gives a server_dial agent the default
// listen address. A listen address that is a bare port gains its colon.
func (c Config) Normalized() Config {
	c.Server = strings.TrimRight(strings.TrimSpace(c.Server), "/")
	c.Token = strings.TrimSpace(c.Token)
	mode := strings.TrimSpace(c.ConnectMode)
	if mode == "" {
		mode = AgentDial
	}
	c.ConnectMode = mode
	c.Listen = NormalizeListen(c.Listen)
	if c.Listen == "" && c.ConnectMode == ServerDial {
		c.Listen = DefaultListen
	}
	c.RuntimeDriver = strings.TrimSpace(c.RuntimeDriver)
	c.MountRoot = strings.TrimSpace(c.MountRoot)
	c.UdockerBundle = strings.TrimSpace(c.UdockerBundle)
	return c
}

// NormalizeListen trims a listen address and turns a bare port into :port.
func NormalizeListen(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen != "" && strings.Trim(listen, "0123456789") == "" {
		return ":" + listen
	}
	return listen
}

// Validate says whether the agent can start with c, by one rule shared by
// the agent and the server: a token always; a server URL when the agent dials
// the server (when given anyway it must still be one); a listen address when
// the server dials the agent.
func (c Config) Validate() error {
	c = c.Normalized()
	for _, value := range c.values() {
		if hasControl(value) {
			return ErrValueInvalid
		}
	}
	if c.ConnectMode != AgentDial && c.ConnectMode != ServerDial {
		return fmt.Errorf("%w (got %q)", ErrModeInvalid, c.ConnectMode)
	}
	if c.Token == "" {
		return ErrTokenRequired
	}
	if c.Server == "" {
		if c.ConnectMode == AgentDial {
			return ErrServerRequired
		}
	} else if err := ValidateServer(c.Server); err != nil {
		return err
	}
	if c.ConnectMode == ServerDial {
		if _, err := ListenPort(c.Listen); err != nil {
			return err
		}
	}
	return nil
}

// ValidateServer accepts an http(s) URL with a host and nothing but a path:
// no credentials, query or fragment.
func ValidateServer(server string) error {
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrServerInvalid, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: it must start with http:// or https:// (got %q)", ErrServerInvalid, server)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("%w: it must be scheme://host[:port][/path] (got %q)", ErrServerInvalid, server)
	}
	return nil
}

// ListenPort is the port of a listen address (host:port or :port).
func ListenPort(listen string) (int, error) {
	_, port, err := net.SplitHostPort(NormalizeListen(listen))
	if err != nil {
		return 0, fmt.Errorf("%w (got %q)", ErrListenInvalid, listen)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%w (got %q)", ErrListenInvalid, listen)
	}
	return n, nil
}

// field is one KEY=value line of the file, in the order Render writes them.
type field struct {
	key string
	ptr func(*Config) *string
}

var fields = []field{
	{"SERVER", func(c *Config) *string { return &c.Server }},
	{"TOKEN", func(c *Config) *string { return &c.Token }},
	{"CONNECT_MODE", func(c *Config) *string { return &c.ConnectMode }},
	{"LISTEN", func(c *Config) *string { return &c.Listen }},
	{"RUNTIME_DRIVER", func(c *Config) *string { return &c.RuntimeDriver }},
	{"MOUNT_ROOT", func(c *Config) *string { return &c.MountRoot }},
	{"UDOCKER_BUNDLE", func(c *Config) *string { return &c.UdockerBundle }},
}

func (c Config) values() []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, *f.ptr(&c))
	}
	return out
}

const header = "# gpuworks agent configuration, written by `gpuworks-agent configure`.\n# Every way the agent starts runs `gpuworks-agent run --state <this directory>`.\n"

// Render is the file's content for a valid configuration.
func (c Config) Render() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c = c.Normalized()
	var b bytes.Buffer
	b.WriteString(header)
	for _, f := range fields {
		fmt.Fprintf(&b, "%s=%s\n", f.key, *f.ptr(&c))
	}
	return b.Bytes(), nil
}

// Parse reads a file Render wrote. Values are taken literally: no quoting,
// escapes or variable expansion. Comment and blank lines are skipped; a key
// this version does not know is ignored, so a newer installer's file still
// starts an older agent. Parse does not validate: run applies its flags and
// environment first.
func Parse(data []byte) (Config, error) {
	var c Config
	known := map[string]func(*Config) *string{}
	for _, f := range fields {
		known[f.key] = f.ptr
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.TrimSpace(key) != key {
			return Config{}, fmt.Errorf("%s line %d: want KEY=value", FileName, i+1)
		}
		if hasControl(value) {
			return Config{}, fmt.Errorf("%s line %d: %w", FileName, i+1, ErrValueInvalid)
		}
		if ptr, ok := known[key]; ok {
			*ptr(&c) = value
		}
	}
	return c, nil
}

// Path is the configuration file of the agent whose state directory is dir.
func Path(stateDir string) string { return filepath.Join(stateDir, FileName) }

// Load reads the configuration in stateDir. A missing file is reported with
// an error that satisfies errors.Is(err, os.ErrNotExist).
func Load(stateDir string) (Config, error) {
	data, err := os.ReadFile(Path(stateDir))
	if err != nil {
		return Config{}, err
	}
	return Parse(data)
}

// Write replaces the configuration in stateDir, readable by its owner only
// (it holds the token). The file is written beside its final name and
// renamed, so an agent starting meanwhile reads the old file or the new one.
func Write(stateDir string, c Config) error {
	data, err := c.Render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, "."+FileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), Path(stateDir))
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
