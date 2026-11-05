package main

import (
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
)

const connectionHistoryBytes = 512 << 10
const connectionHistoryEvents = 32

var connectionHistoryAttempt = regexp.MustCompile(`^[a-f0-9]{16,64}$`)
var connectionHistoryStages = map[string]bool{"configuration": true, "dial": true, "dns": true, "connect": true, "proxy_connect": true, "tls": true, "ws_upgrade": true, "yamux": true, "hello": true, "wait_registered": true, "server_hello": true, "session": true}
var connectionHistoryReasons = map[string]bool{"bad_token": true, "proto_mismatch": true, "stale_session": true, "dns": true, "connect": true, "tls": true, "ws_upgrade": true, "timeout": true, "cancelled": true, "transport": true, "invalid_configuration": true, "none": true}

func addConnectionHistory(report *proto.DoctorReport, state, token string) {
	secrets := []string{token}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if u, err := url.Parse(os.Getenv(name)); err == nil && u.User != nil {
			if password, ok := u.User.Password(); ok && password != "" {
				secrets = append(secrets, password)
			}
		}
	}
	history, status := readConnectionHistory(state, secrets)
	putConnectionFact(report, "history_state", status)
	if history == nil {
		return
	}
	for _, key := range []string{"at", "reason", "stage", "attempt", "lane", "server_url", "retry_at"} {
		if value := history[key]; value != "" {
			putConnectionFact(report, "last_failure."+key, value)
		}
	}
	putConnectionFact(report, "last_failure.source", "agent_log")
}

// Only these two known regular files are read. Tail reads share a hard byte
// budget; an incomplete boundary line cannot masquerade as a complete event.
func readConnectionHistory(state string, secrets []string) (map[string]string, string) {
	remaining, count := connectionHistoryBytes, 0
	unavailable := false
	var newest map[string]string
	var newestAt time.Time
	for _, name := range []string{"agent.log", "agent.log.1"} {
		if remaining == 0 || count >= connectionHistoryEvents {
			break
		}
		lines, used, err := connectionLogTail(filepath.Join(state, name), remaining)
		remaining -= used
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				unavailable = true
			}
			continue
		}
		for i := len(lines) - 1; i >= 0 && count < connectionHistoryEvents; i-- {
			event := parseConnectionEvent(lines[i], secrets)
			if event == nil {
				continue
			}
			count++
			if event["outcome"] != "failed" {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, event["observed_at"])
			if newest == nil || at.After(newestAt) {
				newestAt = at
				newest = map[string]string{"at": event["observed_at"], "reason": event["reason"], "stage": event["stage"], "attempt": event["attempt"], "lane": event["lane"]}
				// These belong to the selected historical event, never the current probe.
				for _, key := range []string{"server_url", "retry_at"} {
					if value := event[key]; value != "" {
						newest[key] = value
					}
				}
			}
		}
	}
	if newest != nil {
		return newest, "found"
	}
	if unavailable {
		return nil, "unavailable"
	}
	return nil, "unknown"
}

func connectionLogTail(path string, limit int) ([]string, int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("not a regular connection log")
	}
	// Nonblocking also bounds a malicious regular-file-to-FIFO swap between
	// Lstat and Open; SameFile and Stat below reject replacement/special files.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return nil, 0, errors.New("connection log changed")
	}
	start := max(current.Size()-int64(limit), 0)
	if _, err = file.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)))
	if err != nil {
		return nil, len(data), err
	}
	text := string(data)
	if start > 0 {
		if cut := strings.IndexByte(text, '\n'); cut >= 0 {
			text = text[cut+1:]
		} else {
			return nil, len(data), nil
		}
	}
	// Only newline-terminated complete events are trustworthy.
	end := strings.LastIndexByte(text, '\n')
	if end < 0 {
		return nil, len(data), nil
	}
	return strings.Split(text[:end], "\n"), len(data), nil
}

func parseConnectionEvent(line string, secrets []string) map[string]string {
	if len(line) > 4096 || !strings.Contains(line, "msg=connection_event_v1") {
		return nil
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(line, secret) {
			return nil
		}
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "source": true, "stage": true, "outcome": true, "reason": true, "attempt": true, "lane": true, "observed_at": true, "server_url": true, "retry_at": true, "suppressed_retries": true}
	values := map[string]string{}
	for _, field := range strings.Fields(line) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || !allowed[key] || value == "" {
			return nil
		}
		if strings.HasPrefix(value, "\"") {
			decoded, err := strconv.Unquote(value)
			if err != nil {
				return nil
			}
			value = decoded
		}
		// Validate after every decoding boundary: quoted log fields can hide a
		// credential that was absent from the raw line.
		for _, secret := range secrets {
			if secret != "" && strings.Contains(value, secret) {
				return nil
			}
		}
		if key == "server_url" {
			parsed, err := url.Parse(value)
			if err != nil {
				return nil
			}
			for _, secret := range secrets {
				if secret != "" && (strings.Contains(parsed.Path, secret) || strings.Contains(parsed.Host, secret)) {
					return nil
				}
			}
		}
		if value == "" || strings.ContainsAny(value, "\"'\\") || strings.IndexFunc(value, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
			return nil
		}
		if _, duplicate := values[key]; duplicate {
			return nil
		}
		values[key] = value
	}
	if values["msg"] != "connection_event_v1" || values["source"] != "agent_runtime" || !connectionHistoryStages[values["stage"]] || !connectionHistoryReasons[values["reason"]] || !connectionHistoryAttempt.MatchString(values["attempt"]) {
		return nil
	}
	if values["lane"] != "ctrl" && values["lane"] != "data" && values["lane"] != "single" {
		return nil
	}
	if values["outcome"] != "failed" && values["outcome"] != "ok" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339Nano, values["observed_at"]); err != nil {
		return nil
	}
	if value := values["retry_at"]; value != "" {
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return nil
		}
	}
	if value := values["server_url"]; value != "" {
		if agentconf.ValidateServer(value) != nil {
			return nil
		}
	}
	if value := values["suppressed_retries"]; value != "" {
		for _, r := range value {
			if r < '0' || r > '9' {
				return nil
			}
		}
		if _, err := strconv.ParseUint(value, 10, 64); err != nil {
			return nil
		}
	}
	return values
}
