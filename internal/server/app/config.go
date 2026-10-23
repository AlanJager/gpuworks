package app

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/selflog"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// Config holds server runtime configuration. cmd/gpuworks fills it from
// server.yaml and the command line; nothing here reads the environment.
type Config struct {
	// SelfLog captures the server's own error-level slog records into the
	// logstore under source=server. Nil disables that drain.
	SelfLog *selflog.Collector

	// Addr is one address serving every function. It is used when Listen is
	// empty: development, tests, and a --addr override.
	Addr string
	// Listen binds each function to its own addresses (server.yaml).
	Listen serverconfig.Listen
	// ConfigPath is server.yaml; empty means the listen addresses cannot be
	// edited from the console.
	ConfigPath string
	// ListenOverridden is set when --addr replaced the file's listen section,
	// so the console shows it read-only.
	ListenOverridden bool
	// Host is the machine's addresses; zero means the running machine.
	Host serverconfig.Host
	// SelfRestart is set when a service manager starts the process again after
	// it exits, so the console can restart it by exiting.
	SelfRestart bool
	// UpdateChannel is server.yaml's update_channel: the distribution channel
	// for update checks, official catalog updates and console upgrades; empty
	// follows the channel this binary was built for.
	UpdateChannel string
	// ManualUpdateChecks is server.yaml's auto_check_updates: false. The zero
	// value checks by itself, which is what every harness expects.
	ManualUpdateChecks bool
	// UsageReportOff is server.yaml's usage_report: false. The zero value
	// takes part in the user experience improvement program.
	UsageReportOff bool

	DataDir                   string // base dir for db and future tsdb/logs
	DBPath                    string // sqlite path (derived from DataDir if empty)
	VLLMImage                 string // default vllm image for deployments
	AdminUser                 string // bootstrap admin username
	AdminPassword             string // bootstrap admin password for in-process harnesses
	UdockerBundleSource       string // optional local udocker offline bundle source for SSH installs
	SSHProxy                  string // SOCKS5 proxy (socks5://host:port) for SSH to nodes
	ModelEndpoint             string // optional HF/ModelScope endpoint override for in-process harnesses
	TrustedProxies            string // comma-separated proxy IPs/CIDRs allowed to supply X-Forwarded-* headers
	TLSCertFile               string
	TLSKeyFile                string
	OutboundAllowPrivateHosts []string
	StorageProbeRoots         []string

	SessionIdleTimeout time.Duration // inactivity deadline for management sessions
	SessionMaxLifetime time.Duration // absolute login deadline

	ProxyMaxBodyBytes int64 // max /v1 request body spooled to disk before routing (413 beyond)

	RecoveryEnabled             bool
	RecoveryGracePeriod         time.Duration
	RecoveryInitialInterval     time.Duration
	RecoveryBackoffMultiplier   float64
	RecoveryMaxInterval         time.Duration
	RecoveryRestartWait         time.Duration
	StaleCheckInterval          time.Duration
	StaleAfter                  time.Duration
	ServerDialReconnectInterval time.Duration
	// MetricsRetention bounds how long raw tsdb samples (and logstore rows) are
	// kept on a first start, the data retention setting's initial value for
	// both (opsStats); the setting decides from then on.
	MetricsRetention time.Duration
}

// DefaultConfig is the configuration before server.yaml and the command line
// are applied.
func DefaultConfig() Config {
	return Config{
		Addr:                        ":" + serverconfig.DefaultPort,
		DataDir:                     "./data",
		VLLMImage:                   "vllm/vllm-openai:latest",
		AdminUser:                   "admin",
		SessionIdleTimeout:          24 * time.Hour,
		SessionMaxLifetime:          7 * 24 * time.Hour,
		ProxyMaxBodyBytes:           proxy.DefaultMaxBodyBytes,
		RecoveryEnabled:             true,
		RecoveryGracePeriod:         2 * time.Minute,
		RecoveryInitialInterval:     30 * time.Second,
		RecoveryBackoffMultiplier:   2,
		RecoveryMaxInterval:         10 * time.Minute,
		RecoveryRestartWait:         time.Minute,
		StaleCheckInterval:          30 * time.Second,
		StaleAfter:                  90 * time.Second,
		ServerDialReconnectInterval: 10 * time.Second,
		MetricsRetention:            72 * time.Hour,
		StorageProbeRoots:           []string{"/opt/gpuworks/models", "/opt/gpuworks/cache", "/mnt/gpuworks"},
	}
}

// bindings are the sockets this configuration opens.
func (c Config) bindings() []serverconfig.Binding {
	if !c.Listen.Empty() {
		return serverconfig.Bindings(c.Listen)
	}
	return []serverconfig.Binding{{Addr: c.Addr, Functions: serverconfig.Functions}}
}

// masterSecret returns the secret used to derive the credential-encryption key:
// the file in the data dir, generated once.
func masterSecret(cfg Config) string {
	path := filepath.Join(cfg.DataDir, "master.key")
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		return string(b)
	}
	secret := randHex(32)
	// 0664 on purpose: the operator backs this file up without sudo. The
	// install tree is shared workspace — group write, others read.
	_ = os.WriteFile(path, []byte(secret), 0o664)
	return secret
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "gpuworks-fallback-master-secret"
	}
	return hex.EncodeToString(b)
}

func normalizeRuntimeConfig(cfg Config) Config {
	policy := (auth.SessionPolicy{IdleTimeout: cfg.SessionIdleTimeout, MaxLifetime: cfg.SessionMaxLifetime}).Normalized()
	cfg.SessionIdleTimeout, cfg.SessionMaxLifetime = policy.IdleTimeout, policy.MaxLifetime
	if cfg.RecoveryGracePeriod <= 0 {
		cfg.RecoveryGracePeriod = 2 * time.Minute
	}
	if cfg.RecoveryInitialInterval <= 0 {
		cfg.RecoveryInitialInterval = 30 * time.Second
	}
	if cfg.RecoveryBackoffMultiplier < 1 {
		cfg.RecoveryBackoffMultiplier = 2
	}
	if cfg.RecoveryMaxInterval <= 0 {
		cfg.RecoveryMaxInterval = 10 * time.Minute
	}
	if cfg.RecoveryRestartWait <= 0 {
		cfg.RecoveryRestartWait = time.Minute
	}
	if cfg.StaleCheckInterval <= 0 {
		cfg.StaleCheckInterval = 30 * time.Second
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 90 * time.Second
	}
	if cfg.ServerDialReconnectInterval <= 0 {
		cfg.ServerDialReconnectInterval = 10 * time.Second
	}
	if cfg.ProxyMaxBodyBytes <= 0 {
		cfg.ProxyMaxBodyBytes = proxy.DefaultMaxBodyBytes
	}
	if cfg.MetricsRetention <= 0 {
		cfg.MetricsRetention = 72 * time.Hour
	}
	return cfg
}
