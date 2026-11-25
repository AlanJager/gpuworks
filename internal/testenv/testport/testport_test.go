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
	for range 50 {
		port := Port(t, "127.0.0.1")
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
}
