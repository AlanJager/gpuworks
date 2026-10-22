package netinfo

import (
	"errors"
	"net"
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDumpTCPListenersSeesRealListenersOfBothFamilies(t *testing.T) {
	var want []int
	for _, addr := range []struct{ network, host string }{{"tcp4", "127.0.0.1:0"}, {"tcp6", "[::1]:0"}} {
		ln, err := net.Listen(addr.network, addr.host)
		if err != nil {
			if addr.network == "tcp6" {
				continue // an IPv6-less host has no tcp6 listener to find
			}
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		want = append(want, ln.Addr().(*net.TCPAddr).Port)
	}
	// The client end of an established connection is not a listener.
	peer, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp4", peer.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
	clientPort := conn.LocalAddr().(*net.TCPAddr).Port

	got, err := dumpTCPListeners()
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPROTONOSUPPORT) {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("sock_diag unavailable: %v", err)
		}
		t.Skipf("sock_diag unavailable in this sandbox: %v", err)
	}
	if err != nil {
		t.Fatalf("dumpTCPListeners: %v", err)
	}
	for _, p := range want {
		if !slices.Contains(got, p) {
			t.Fatalf("listeners %v miss port %d", got, p)
		}
	}
	if slices.Contains(got, clientPort) {
		t.Fatalf("listeners %v include the established client port %d", got, clientPort)
	}
}
