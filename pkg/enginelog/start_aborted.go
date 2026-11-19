package enginelog

import (
	"regexp"
	"strings"
)

// An SGLang scheduler that raises logs "Scheduler hit an exception" and
// signals the server, which logs "SIGQUIT received ... one child failed" and
// kills its process tree. The engine does not come back from that. Normally
// the container exits with it; when the kill does not end the server process
// (it is the container's init), an HTTP process stays behind that answers its
// health check as not ready until the start timeout.
var sglangStartAbortedRE = regexp.MustCompile(`\] Scheduler hit an exception:|\] SIGQUIT received\..*one child failed`)

// StartAborted reports that the output of an engine of dialect says, in the
// engine's own words, that one of its processes failed and that it is
// shutting itself down. Only the SGLang dialect has such a statement. cause
// is the exception the failed process raised (the last line a traceback ends
// with), empty when the output shows none.
func StartAborted(dialect, logText string) (cause string, aborted bool) {
	if dialect != DialectSGLang {
		return "", false
	}
	lines := strings.Split(ansiRE.ReplaceAllString(logText, ""), "\n")
	for _, line := range lines {
		if sglangStartAbortedRE.MatchString(line) {
			aborted = true
			break
		}
	}
	if !aborted {
		return "", false
	}
	for i := len(lines) - 1; i >= max(0, len(lines)-rootCauseScan); i-- {
		line := stripLinePrefix(lines[i])
		if exceptionLineRE.MatchString(line) && !isWrapperCause(line) {
			return bound(line), true
		}
	}
	return "", true
}
