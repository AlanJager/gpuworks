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
// never assigns a port by itself: 10000 up to the start of the ephemeral
// range, and no further than 29999, where the test harness's engine port
// ranges begin.
//
// What that does and does not give:
//
//   - A port is checked by binding it and releasing it again, so this is
//     still check-then-use. Between the check and the test's own bind, only
//     a process that asks for that port by number can take it.
//   - A port is handed out once per process. `go test` runs each package as
//     its own process, and CI runs several jobs on one host network, each in
//     its own container; none of them know of each other. To keep them apart,
//     a process starts at a random place in the pool and hands out the ports
//     that follow, so each uses one short run of ports. Two processes hand
//     out the same port only when their runs overlap and the second checks
//     the port before the first has bound it.
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
)

var (
	mu    sync.Mutex
	next  = -1 // the next candidate, as an offset into the pool
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
	size := poolEnd() - poolStart
	mu.Lock()
	defer mu.Unlock()
	if next < 0 {
		next = rand.IntN(size)
	}
	for range size {
		port := poolStart + next
		next = (next + 1) % size
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
	t.Fatalf("testport: no free port on %s in %d-%d", ip, poolStart, poolStart+size-1)
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
