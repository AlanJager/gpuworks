package dockerproxy

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// proxyEnv is the proxy a systemd drop-in puts in the daemon's environment.
type proxyEnv struct {
	HTTP, HTTPS, No string
}

func (p proxyEnv) empty() bool { return p.HTTP == "" && p.HTTPS == "" && p.No == "" }

// desiredProxy is the daemon environment a node's network config asks for:
// the configured proxy, and the hosts that must never go through it. A proxy
// given for one scheme serves the other too, as every other pull path here
// treats it. serverURL is the control plane's address; the agent must keep
// reaching it directly, so its host is always bypassed.
func desiredProxy(cfg proto.NetworkConfig, serverURL string) proxyEnv {
	httpProxy, httpsProxy := strings.TrimSpace(cfg.HTTPProxy), strings.TrimSpace(cfg.HTTPSProxy)
	if httpProxy == "" && httpsProxy == "" {
		return proxyEnv{}
	}
	if httpProxy == "" {
		httpProxy = httpsProxy
	}
	if httpsProxy == "" {
		httpsProxy = httpProxy
	}
	return proxyEnv{HTTP: httpProxy, HTTPS: httpsProxy, No: NoProxyFor(cfg.NoProxy, serverURL)}
}

// NoProxyFor is the NO_PROXY the agent writes: the operator's list, then
// loopback, then the server's host, without repeats.
func NoProxyFor(userNoProxy, serverURL string) string {
	var out []string
	seen := map[string]bool{}
	add := func(entry string) {
		entry = strings.TrimSpace(entry)
		if entry == "" || seen[strings.ToLower(entry)] {
			return
		}
		seen[strings.ToLower(entry)] = true
		out = append(out, entry)
	}
	for _, entry := range strings.Split(userNoProxy, ",") {
		add(entry)
	}
	for _, entry := range []string{"localhost", "127.0.0.1", "::1"} {
		add(entry)
	}
	add(hostOf(serverURL))
	return strings.Join(out, ",")
}

// hostOf is the host name of a server URL (no port, no brackets), or "".
func hostOf(serverURL string) string {
	serverURL = strings.TrimSpace(serverURL)
	if serverURL == "" {
		return ""
	}
	if !strings.Contains(serverURL, "://") {
		serverURL = "https://" + serverURL
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

// renderDropIn is the systemd drop-in that gives the daemon its proxy. systemd
// reads quotes, backslashes and %-specifiers in Environment=, so values are
// escaped for it.
func renderDropIn(p proxyEnv) (string, error) {
	var entries []string
	for _, kv := range [][2]string{{"HTTP_PROXY", p.HTTP}, {"HTTPS_PROXY", p.HTTPS}, {"NO_PROXY", p.No}} {
		if kv[1] == "" {
			continue
		}
		if strings.ContainsAny(kv[1], "\r\n\x00") {
			return "", fmt.Errorf("%s contains a control character", kv[0])
		}
		entries = append(entries, `"`+kv[0]+"="+escapeEnv(kv[1])+`"`)
	}
	return "# Managed by the gpuworks agent. Changes are overwritten when the node's pull proxy (Settings → Pull settings → Proxies) changes.\n" +
		"[Service]\nEnvironment=" + strings.Join(entries, " ") + "\n", nil
}

func escapeEnv(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return strings.ReplaceAll(v, "%", "%%")
}

// parseDropIn reads back what renderDropIn wrote. Unknown content is ignored,
// so a hand-edited file reads as whatever proxy it still sets.
func parseDropIn(content string) proxyEnv {
	var p proxyEnv
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "Environment=")
		if !ok {
			continue
		}
		for _, token := range splitQuoted(rest) {
			key, value, ok := strings.Cut(token, "=")
			if !ok {
				continue
			}
			switch key {
			case "HTTP_PROXY":
				p.HTTP = value
			case "HTTPS_PROXY":
				p.HTTPS = value
			case "NO_PROXY":
				p.No = value
			}
		}
	}
	return p
}

// splitQuoted splits a systemd Environment= value into its words, undoing the
// quoting and the escapes renderDropIn applies.
func splitQuoted(s string) []string {
	var words []string
	var cur strings.Builder
	inQuote, started := false, false
	flush := func() {
		if started {
			words = append(words, cur.String())
		}
		cur.Reset()
		started = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			started = true
		case c == '%' && i+1 < len(s) && s[i+1] == '%':
			i++
			cur.WriteByte('%')
			started = true
		case c == '"':
			inQuote = !inQuote
			started = true
		case (c == ' ' || c == '\t') && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return words
}

// redact removes the credentials from a proxy URL so it can be logged.
func redact(proxy string) string {
	raw := proxy
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable)"
	}
	u.User = nil
	return u.String()
}
