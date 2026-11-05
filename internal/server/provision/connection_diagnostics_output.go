package provision

import "strings"

const connectionDiagnosticLimit = 4096

// The SSH copier sees successful writes even after the cap, so an untrusted
// peer cannot grow memory without bound or block cancellation on a short write.
type connectionDiagnosticOutput struct {
	text     strings.Builder
	overflow bool
}

func (o *connectionDiagnosticOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := connectionDiagnosticLimit - o.text.Len()
	if len(p) > remaining {
		p = p[:remaining]
		o.overflow = true
	}
	_, _ = o.text.Write(p)
	return n, nil
}

func (o *connectionDiagnosticOutput) String() string {
	if o.overflow {
		return "diagnostic output exceeded its limit"
	}
	return o.text.String()
}
