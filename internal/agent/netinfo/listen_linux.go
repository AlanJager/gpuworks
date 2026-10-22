package netinfo

import (
	"errors"

	"golang.org/x/sys/unix"
)

// dumpTCPListeners asks the kernel for the listening TCP sockets of both
// address families. An IPv6-less kernel fails only its own family.
func dumpTCPListeners() ([]int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	// The kernel answers a dump at once; the timeout only guards the status
	// loop against a reply that never finishes.
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 2}); err != nil {
		return nil, err
	}
	var ports []int
	var familyErrs []error
	buf := make([]byte, 64<<10)
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := unix.Sendto(fd, diagRequest(family), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
			return ports, err
		}
		for done := false; !done; {
			n, _, err := unix.Recvfrom(fd, buf, 0)
			if err != nil {
				// A half-read dump would leave replies queued for the next
				// family's request, so a socket error ends the collection.
				return ports, err
			}
			done, err = parseDiagDump(buf[:n], func(p int) { ports = append(ports, p) })
			var errno unix.Errno
			if errors.As(err, &errno) {
				familyErrs = append(familyErrs, err) // the kernel refused this family only
			} else if err != nil {
				return ports, err
			}
		}
	}
	return ports, errors.Join(familyErrs...)
}
