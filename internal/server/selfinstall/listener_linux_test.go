//go:build linux

package selfinstall

import (
	"net"
	"os"
	"testing"
)

func TestListenerPIDFindsTheProcessHoldingAPort(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		ln, err := net.Listen(network, map[string]string{"tcp4": "127.0.0.1:0", "tcp6": "[::1]:0"}[network])
		if err != nil {
			if network == "tcp6" {
				t.Logf("no IPv6 loopback: %v", err)
				continue
			}
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		if got := listenerPID(port); got != os.Getpid() {
			t.Errorf("%s: listenerPID(%s) = %d, want %d", network, port, got, os.Getpid())
		}
		_ = ln.Close()
		if got := listenerPID(port); got != 0 {
			t.Errorf("%s: listenerPID(%s) after close = %d, want 0", network, port, got)
		}
	}
	if got := listenerPID("not-a-port"); got != 0 {
		t.Errorf("listenerPID(not-a-port) = %d", got)
	}
}
