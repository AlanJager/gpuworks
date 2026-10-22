package netinfo

import (
	"encoding/binary"
	"errors"
	"sort"
	"syscall"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ListenPorts reports the TCP ports in r that a host socket listens on. The
// kernel's sock_diag netlink interface returns only LISTEN sockets, so the cost
// tracks the listener count rather than every connection on the host. Ports
// found before an error are still returned with it.
func ListenPorts(r proto.PortRange) ([]int, error) {
	return listenPortsIn(r, dumpTCPListeners)
}

func listenPortsIn(r proto.PortRange, dump func() ([]int, error)) ([]int, error) {
	all, err := dump()
	seen := map[int]bool{}
	ports := []int{}
	for _, p := range all {
		if r.Contains(p) && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	return ports, err
}

// Wire constants from linux/netlink.h, linux/sock_diag.h and
// linux/inet_diag.h, spelled out so the codec builds and tests on any OS.
const (
	nlmsgHdrLen      = 16
	nlmsgError       = 2
	nlmsgDone        = 3
	nlmFRequest      = 0x1
	nlmFDump         = 0x300
	sockDiagByFamily = 20
	ipprotoTCP       = 6
	tcpListen        = 10
	inetDiagReqLen   = 56 // struct inet_diag_req_v2
	inetDiagMsgLen   = 72 // struct inet_diag_msg
)

var errTruncated = errors.New("sock_diag: truncated netlink message")

// diagRequest is a dump request for the family's TCP sockets in LISTEN state.
func diagRequest(family uint8) []byte {
	b := make([]byte, nlmsgHdrLen+inetDiagReqLen)
	ne := binary.NativeEndian
	ne.PutUint32(b[0:], uint32(len(b)))
	ne.PutUint16(b[4:], sockDiagByFamily)
	ne.PutUint16(b[6:], nlmFRequest|nlmFDump)
	b[16] = family
	b[17] = ipprotoTCP
	ne.PutUint32(b[20:], 1<<tcpListen) // idiag_states
	return b
}

// parseDiagDump reads one netlink datagram of a dump reply, passing each
// listening socket's local port to add. done reports the end of the dump,
// whether it completed or the kernel returned an error.
func parseDiagDump(b []byte, add func(port int)) (done bool, err error) {
	ne := binary.NativeEndian
	for len(b) >= nlmsgHdrLen {
		n := int(ne.Uint32(b[0:]))
		if n < nlmsgHdrLen || n > len(b) {
			return true, errTruncated
		}
		payload := b[nlmsgHdrLen:n]
		switch ne.Uint16(b[4:]) {
		case nlmsgDone:
			return true, nil
		case nlmsgError:
			if len(payload) < 4 {
				return true, errTruncated
			}
			if code := int32(ne.Uint32(payload)); code != 0 {
				return true, syscall.Errno(-code)
			}
			return true, nil
		case sockDiagByFamily:
			// inet_diag_msg: family, state, timer, retrans, then the socket id
			// whose first field is the local port in network byte order.
			if len(payload) >= inetDiagMsgLen && payload[1] == tcpListen {
				add(int(binary.BigEndian.Uint16(payload[4:])))
			}
		}
		b = b[min((n+3)&^3, len(b)):]
	}
	return false, nil
}
