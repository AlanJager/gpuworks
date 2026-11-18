package proto

import (
	"net/url"
	"strings"
)

// RuntimeProxyStatus is what an agent observes about the proxy its container
// runtime daemon uses, and what it can do about it.
type RuntimeProxyStatus struct {
	// HTTPProxy, HTTPSProxy and NoProxy are what the daemon reports it uses
	// (docker info). A daemon masks proxy credentials, so a password in them
	// reads as "xxxxx".
	HTTPProxy  string `json:"http_proxy"`
	HTTPSProxy string `json:"https_proxy"`
	NoProxy    string `json:"no_proxy"`
	// Configured is the proxy the daemon starts with: the docker unit's
	// environment as systemd has it loaded, every drop-in included, masked
	// like HTTPProxy. ConfiguredKnown is false when systemd could not be
	// asked. A daemon reporting something else has not been restarted since
	// its configuration changed.
	ConfiguredHTTPProxy  string `json:"configured_http_proxy,omitempty"`
	ConfiguredHTTPSProxy string `json:"configured_https_proxy,omitempty"`
	ConfiguredKnown      bool   `json:"configured_known,omitempty"`
	// Written is the proxy the agent last wrote to the daemon's systemd
	// drop-in (empty when it wrote none), so a mismatch with the daemon's own
	// report means the daemon has not been restarted since.
	WrittenHTTPProxy  string `json:"written_http_proxy,omitempty"`
	WrittenHTTPSProxy string `json:"written_https_proxy,omitempty"`
	WrittenNoProxy    string `json:"written_no_proxy,omitempty"`
	// LiveRestore is the daemon's live-restore: containers survive a daemon
	// restart.
	LiveRestore bool `json:"live_restore"`
	// DaemonJSONProxy: /etc/docker/daemon.json sets "proxies", which the
	// daemon prefers over the systemd environment the agent writes.
	DaemonJSONProxy bool `json:"daemon_json_proxy"`
	// Systemd: the daemon runs as a systemd unit the agent can configure.
	Systemd bool `json:"systemd"`
	// Privilege is how the agent can change the daemon: "root", "sudo"
	// (passwordless sudo) or "none".
	Privilege string `json:"privilege"`
	// Error is why the agent could not observe or configure the daemon.
	Error string `json:"error,omitempty"`
}

// MaskProxyCredentials spells a proxy URL the way the Docker daemon reports
// it in `docker info`: any user information becomes "xxxxx:xxxxx".
func MaskProxyCredentials(proxy string) string {
	proxy = strings.TrimSpace(proxy)
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.User == nil {
		return proxy
	}
	parsed.User = url.UserPassword("xxxxx", "xxxxx")
	return parsed.String()
}

// Runtime proxy privileges (RuntimeProxyStatus.Privilege).
const (
	RuntimePrivilegeRoot = "root"
	RuntimePrivilegeSudo = "sudo"
	RuntimePrivilegeNone = "none"
)
