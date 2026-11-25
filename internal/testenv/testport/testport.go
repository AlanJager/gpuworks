// Package testport hands tests loopback addresses to bind later.
//
// A test that needs an address before the code under test binds it (the
// address goes into a configuration, a certificate, a cluster's records)
// cannot hold a listener and pass it on. Listening on port 0 and closing gives
// such an address, but from the kernel's ephemeral range, which every outgoing
// connection and every other port-0 listener on the machine draws from: on a
// CI runner whose jobs share the host network the port is often taken again
// before the test binds it ("bind: address already in use").
//
// The addresses here come from below the ephemeral range, where the kernel
// never assigns a port by itself. Another process can still bind one by
// number; tests in another container that use this package draw at random
// from the same twenty thousand ports and are not coordinated with.
package testport

import (
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The pool ends where the test harness's engine port ranges begin
// (internal/testenv, 30000 and up) or where the ephemeral range begins,
// whichever is lower.
const (
	poolStart   = 10000
	poolEndMost = 30000
	attempts    = 200
)

var (
	mu    sync.Mutex
	given = map[int]struct{}{}
)

// Address returns a 127.0.0.1 address that was free when it was chosen and
// that this process hands out only once.
func Address(t testing.TB) string {
	t.Helper()
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(Port(t, "127.0.0.1")))
}

// Port returns a port that was free on ip when it was chosen and that this
// process hands out only once.
func Port(t testing.TB, ip string) int {
	t.Helper()
	end := poolEnd()
	mu.Lock()
	defer mu.Unlock()
	for range attempts {
		port := poolStart + rand.IntN(end-poolStart)
		if _, taken := given[port]; taken {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		if err := listener.Close(); err != nil {
			t.Fatalf("testport: release %s:%d: %v", ip, port, err)
		}
		given[port] = struct{}{}
		return port
	}
	t.Fatalf("testport: no free port on %s in %d-%d after %d attempts", ip, poolStart, end-1, attempts)
	return 0
}

// poolEnd is the first port not in the pool.
func poolEnd() int {
	raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return poolEndMost
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return poolEndMost
	}
	low, err := strconv.Atoi(fields[0])
	// A machine whose ephemeral range starts unusually low leaves too little
	// room below it; the fixed pool is then no worse than port 0 was.
	if err != nil || low >= poolEndMost || low < poolStart+1000 {
		return poolEndMost
	}
	return low
}
