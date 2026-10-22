// Package logline classifies single container log lines: it extracts the
// severity the engine stamped on the line and recognizes scraper/access-log
// noise that should not be shipped at all. It is stateless and per-line by
// design — multi-line constructs like Python tracebacks are folded at display
// time, not here.
package logline

import (
	"regexp"
	"strings"
)

// Severity levels as stored and filtered. Empty means the line carried no
// recognizable level and is displayed like plain text.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelFatal = "fatal"
)

// levelTokens matches severity stamps the way engines actually write them:
// "INFO 09-18 10:51:17" (vLLM), "WARNING [config.py:99]" (vLLM),
// "INFO:     127.0.0.1:5-" (uvicorn), "[ERROR]" (Ascend toolkit). Lowercase
// message words like "1 validation error" deliberately do not match: a bare
// "error" inside prose is content, not a stamp.
var levelTokens = regexp.MustCompile(`(?:\A|[^\p{L}\d_])(FATAL|Fatal|ERROR|Error|ERR|CRITICAL|CRIT|PANIC|WARNING|Warning|WARN|INFO|Info|DEBUG|TRACE|NOTICE)(?:\z|[^\p{L}\d_])`)

// levelEquals matches the key=value form used by logrus/zap/serilog text
// encoders: level=error, level=warning, levelno=40.
var levelEquals = regexp.MustCompile(`(?i)\blevel["']?\s*[=:]\s*["']?(error|fatal|critical|panic|warn(?:ing)?|info|debug|trace)\b`)

var levelNormalize = map[string]string{
	"TRACE": LevelDebug, "DEBUG": LevelDebug,
	"INFO": LevelInfo, "NOTICE": LevelInfo,
	"WARN": LevelWarn, "WARNING": LevelWarn,
	"ERROR": LevelError, "ERR": LevelError, "CRIT": LevelError, "CRITICAL": LevelError,
	"PANIC": LevelFatal, "FATAL": LevelFatal,
}

// tracebackHeader marks the first line of a Python traceback; the frames that
// follow carry no stamp of their own, so they stay unclassified here and are
// grouped with it at display time.
var tracebackHeader = regexp.MustCompile(`\bTraceback \(most recent call last\):`)

// Level returns the severity stamped on the line ("" when none). The first
// recognized stamp wins, so a WARNING line that later quotes an ERROR line is
// still a warning.
func Level(line string) string {
	if tracebackHeader.MatchString(line) {
		return LevelError
	}
	if m := levelEquals.FindStringSubmatch(line); m != nil {
		return normalize(m[1])
	}
	for _, m := range levelTokens.FindAllStringSubmatch(line, -1) {
		if lvl := normalize(m[1]); lvl != "" {
			return lvl
		}
	}
	return ""
}

func normalize(token string) string {
	if lvl, ok := levelNormalize[strings.ToUpper(token)]; ok {
		return lvl
	}
	return ""
}

// noisePattern matches engine access logs for endpoints the platform itself
// scrapes every few seconds. In production stores these dominated row counts
// (52% on a live deployment) while carrying zero diagnostic value.
var noisePattern = regexp.MustCompile(`INFO:? +\d{1,3}(?:\.\d{1,3}){3}:\d+ - "(?:GET|POST|HEAD) /(?:metrics|health|healthz|ready|readyz|alive|livez|version|docs|redoc|openapi\.json)(?:[ ?"]| HTTP)`)

// IsNoise reports whether the line is scraper/access-log chatter that the
// agent drops before shipping. Only the scrape endpoints above qualify —
// application endpoints like POST /v1/completions are real traffic and are
// always kept.
func IsNoise(line string) bool {
	return noisePattern.MatchString(line)
}
