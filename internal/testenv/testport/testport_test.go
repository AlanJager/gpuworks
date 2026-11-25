package testport

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Ports come from below the kernel's ephemeral range and below the harness's
// engine ranges, are handed out once, and can be bound afterwards.
func TestPortsAreOutsideTheEphemeralRangeDistinctAndBindable(t *testing.T) {
	ephemeralLow := poolEndMost
	if raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
		if fields := strings.Fields(string(raw)); len(fields) == 2 {
			if low, err := strconv.Atoi(fields[0]); err == nil {
				ephemeralLow = low
			}
		}
	}
	seen := map[int]bool{}
	first := 0
	for i := range 50 {
		port := Port(t, "127.0.0.1")
		if i == 0 {
			first = port
		}
		if port < poolStart || port >= poolEndMost || port >= ephemeralLow {
			t.Fatalf("port %d is outside %d-%d or inside the ephemeral range from %d", port, poolStart, poolEndMost-1, ephemeralLow)
		}
		if seen[port] {
			t.Fatalf("port %d was handed out twice", port)
		}
		seen[port] = true
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("port %d cannot be bound after it was handed out: %v", port, err)
		}
		defer listener.Close()
	}
	// One process uses one short run of ports, so that processes that start
	// at different places stay apart. Ports already in use are skipped, so
	// the run can be a little longer than the fifty handed out.
	size := poolEnd() - poolStart
	for port := range seen {
		if distance := (port - first + size) % size; distance > 500 {
			t.Fatalf("port %d is %d ports after the first, %d: not one run", port, distance, first)
		}
	}
}

// A port something listens on is skipped: the next one handed out is a
// later port of the same run.
func TestPortSkipsAPortInUse(t *testing.T) {
	Port(t, "127.0.0.1") // the run has begun
	mu.Lock()
	size := poolEnd() - poolStart
	upcoming := poolStart + next
	mu.Unlock()
	held, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(upcoming)))
	if err != nil {
		t.Skipf("the next port of the run, %d, is already in use: %v", upcoming, err)
	}
	defer held.Close()
	got := Port(t, "127.0.0.1")
	if distance := (got - upcoming + size) % size; got == upcoming || distance > 500 {
		t.Fatalf("with %d in use the next port handed out was %d", upcoming, got)
	}
}
