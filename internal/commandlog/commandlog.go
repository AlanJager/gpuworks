// Package commandlog provides consistent, credential-safe logging around
// external commands. Callers should create one Execution immediately before
// invoking the command and finish it exactly once with the captured output.
package commandlog

import (
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxOutputTailBytes = 2048

var (
	sensitiveFlag = regexp.MustCompile(`(?i)(--(?:token|password|secret|api[-_]key|access[-_]key|private[-_]key(?:[-_]pem)?)(?:=|\s+))(?:'[^']*'|"[^"]*"|[^\s]+)`)
	sensitiveEnv  = regexp.MustCompile(`(?i)(\b[A-Z0-9_]*(?:TOKEN|PASSWORD|SECRET|API_KEY|ACCESS_KEY|PRIVATE_KEY)=)(?:'[^']*'|"[^"]*"|[^\s]+)`)
	sensitiveURL  = regexp.MustCompile(`(?i)([?&](?:token|password|secret|api[-_]key|access[-_]key)=)[^&\s'"]+`)
	urlUserinfo   = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)
	authHeader    = regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic)\s+)[^\s'"]+`)
	jsonSecretKey = regexp.MustCompile(`(?i)"[^"]*(?:token|password|secret|api[-_]?key|access[-_]?key|private[-_]?key)[^"]*"\s*:`)
	scriptSecret  = regexp.MustCompile(`(?im)(?:^|[;\s])(?:export\s+)?[A-Z0-9_]*(?:TOKEN|PASSWORD|SECRET|API_KEY|ACCESS_KEY|PRIVATE_KEY)\s*=`)
)

const redactedValue = "<redacted>"

type Execution struct {
	command string
	started time.Time
	once    sync.Once
}

func Start(command string) *Execution {
	execution := &Execution{command: Redact(command), started: time.Now()}
	slog.Info("external command started", "command", execution.command)
	return execution
}

func (e *Execution) Finish(output string, err error) {
	e.once.Do(func() {
		attrs := []any{
			"command", e.command,
			"duration", time.Since(e.started).String(),
			"output_bytes", len(output),
		}
		if err != nil {
			attrs = append(attrs, "err", err, "output_tail", outputTail(output))
			slog.Warn("external command failed", attrs...)
			return
		}
		slog.Info("external command finished", attrs...)
	})
}

func Redact(command string) string {
	command = sensitiveFlag.ReplaceAllString(command, `${1}<redacted>`)
	command = sensitiveEnv.ReplaceAllString(command, `${1}<redacted>`)
	command = sensitiveURL.ReplaceAllString(command, `${1}<redacted>`)
	command = urlUserinfo.ReplaceAllString(command, `${1}<redacted>@`)
	return authHeader.ReplaceAllString(command, `${1}<redacted>`)
}

// RedactArgv removes credentials without flattening argv into a command line.
// Keeping the slice shape intact matters to launch observation: callers must
// be able to distinguish executable and argument boundaries after redaction.
func RedactArgv(argv []string) []string {
	if argv == nil {
		return nil
	}
	out := make([]string, len(argv))
	redactNext := false
	redactScriptNext := false
	for i, token := range argv {
		if redactNext {
			out[i] = redactedValue
			redactNext = false
			continue
		}
		if redactScriptNext {
			out[i] = RedactScript(token)
			redactScriptNext = false
			continue
		}
		if key, _, ok := strings.Cut(token, "="); ok && sensitiveName(strings.TrimLeft(key, "-")) {
			out[i] = key + "=" + redactedValue
			continue
		}
		trimmed := strings.TrimSpace(token)
		if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && jsonSecretKey.MatchString(trimmed) {
			out[i] = redactedValue
			continue
		}
		out[i] = Redact(token)
		if strings.HasPrefix(token, "-") && sensitiveName(strings.TrimLeft(token, "-")) {
			redactNext = true
		}
		if token == "-c" && i > 0 && isShellExecutable(argv[i-1]) {
			redactScriptNext = true
		}
	}
	return out
}

// RedactEnv preserves environment key presence while removing sensitive
// values. The returned map never aliases the input.
func RedactEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		if sensitiveName(key) {
			out[key] = redactedValue
			continue
		}
		out[key] = redactStructuredValue(value)
	}
	return out
}

// RedactScript applies text redaction before a script is logged, persisted, or
// returned through an API. Structured argv/env should use their own helpers.
func RedactScript(script string) string {
	// Shell syntax is too broad to safely preserve around a secret assignment:
	// quoted values may span lines, use heredocs, or expand indirectly. Once a
	// sensitive assignment or private-key block is present, redact the script as
	// one value instead of pretending partial regex replacement is complete.
	upper := strings.ToUpper(script)
	if scriptSecret.MatchString(script) || jsonSecretKey.MatchString(script) || strings.Contains(upper, "BEGIN PRIVATE KEY") || strings.Contains(upper, "BEGIN RSA PRIVATE KEY") {
		return redactedValue
	}
	return Redact(script)
}

// RedactStructured clones JSON-shaped data while removing secrets at every
// depth. It is intended for API projections of arbitrary maps and slices where
// neither the producer nor the reader can assume a fixed schema.
func RedactStructured(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if sensitiveName(key) {
				out[key] = redactedValue
				continue
			}
			out[key] = RedactStructured(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, child := range typed {
			out[index] = RedactStructured(child)
		}
		return out
	case string:
		return RedactScript(typed)
	default:
		return value
	}
}

func redactStructuredValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && jsonSecretKey.MatchString(trimmed) {
		return redactedValue
	}
	return Redact(value)
}

func isShellExecutable(value string) bool {
	switch strings.ToLower(filepath.Base(strings.TrimSpace(value))) {
	case "sh", "bash", "zsh", "dash", "ash", "ksh":
		return true
	default:
		return false
	}
}

func sensitiveName(name string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(strings.TrimSpace(name)))
	switch normalized {
	case "token", "password", "secret", "api_key", "access_key", "private_key", "private_key_pem", "authorization":
		return true
	}
	return strings.HasSuffix(normalized, "_token") ||
		strings.HasSuffix(normalized, "_password") ||
		strings.HasSuffix(normalized, "_secret") ||
		strings.HasSuffix(normalized, "_api_key") ||
		strings.HasSuffix(normalized, "_access_key") ||
		strings.HasSuffix(normalized, "_private_key")
}

func outputTail(output string) string {
	output = strings.TrimSpace(output)
	if len(output) <= maxOutputTailBytes {
		return output
	}
	return "..." + output[len(output)-maxOutputTailBytes:]
}
