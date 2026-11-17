package pluginexec

import (
	"sort"
	"strconv"
	"strings"
)

// The descriptors a plugin inherits, in the order the server passes them.
const (
	ServeFD = 3 // the plugin serves the server on it
	HostFD  = 4 // the plugin calls the host API on it
)

// inherited are the server's variables a plugin keeps: the search path and
// time zone, and the proxy and CA settings it needs to reach the network
// the way the server does. Nothing else of the server's environment (its
// configuration, credentials, the database path) reaches a plugin.
var inherited = []string{
	"PATH", "TZ",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// set are the variables the server sets for every plugin.
var set = []string{"HOME", "TMPDIR", "LANG", "GWX_PROTOCOL", "GWX_SERVE_FD", "GWX_HOST_FD"}

// Environ is a plugin's environment: the allowed variables of the server's
// environment, HOME and TMPDIR at the plugin's directories, LANG=C.UTF-8 and
// the gwx/1 variables. protocol is the gwx major.
func Environ(server []string, home, tmp string, protocol int) []string {
	out := filterEnv(server, inherited)
	out = append(out,
		"HOME="+home,
		"TMPDIR="+tmp,
		"LANG=C.UTF-8",
		"GWX_PROTOCOL="+strconv.Itoa(protocol),
		"GWX_SERVE_FD="+strconv.Itoa(ServeFD),
		"GWX_HOST_FD="+strconv.Itoa(HostFD),
	)
	sort.Strings(out)
	return out
}

// allowed is every name a plugin's environment may hold; the trampoline
// drops the rest of what it was given.
func allowed() []string { return append(append([]string(nil), inherited...), set...) }

// filterEnv keeps the entries of env whose name is in names, the first of
// each name only.
func filterEnv(env, names []string) []string {
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !keep[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, kv)
	}
	return out
}
