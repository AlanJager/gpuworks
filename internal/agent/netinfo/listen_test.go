package netinfo

import (
	"encoding/binary"
	"errors"
	"reflect"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// nlmsg frames one netlink message the way the kernel does: native-endian
// header, payload padded to a 4-byte boundary.
func nlmsg(typ uint16, payload []byte) []byte {
	n := nlmsgHdrLen + len(payload)
	b := make([]byte, (n+3)&^3)
	binary.NativeEndian.PutUint32(b[0:], uint32(n))
	binary.NativeEndian.PutUint16(b[4:], typ)
	copy(b[nlmsgHdrLen:], payload)
	return b
}

// diagMsg is an inet_diag_msg for a socket in state bound to port.
func diagMsg(state uint8, port uint16) []byte {
	b := make([]byte, inetDiagMsgLen)
	b[0], b[1] = 2, state
	binary.BigEndian.PutUint16(b[4:], port)
	return b
}

func errMsg(errno syscall.Errno) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, uint32(-int32(errno)))
	return nlmsg(nlmsgError, b)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestDiagRequestAsksForListeningTCPSocketsOfOneFamily(t *testing.T) {
	b := diagRequest(10)
	ne := binary.NativeEndian
	if len(b) != 72 || ne.Uint32(b[0:]) != 72 {
		t.Fatalf("request length = %d / header %d, want 72", len(b), ne.Uint32(b[0:]))
	}
	if typ, flags := ne.Uint16(b[4:]), ne.Uint16(b[6:]); typ != 20 || flags != 0x301 {
		t.Fatalf("type/flags = %d/%#x, want SOCK_DIAG_BY_FAMILY/REQUEST|DUMP", typ, flags)
	}
	if b[16] != 10 || b[17] != 6 || ne.Uint32(b[20:]) != 1<<10 {
		t.Fatalf("family/protocol/states = %d/%d/%#x, want 10/6/LISTEN", b[16], b[17], ne.Uint32(b[20:]))
	}
}

func TestParseDiagDumpKeepsOnlyListeningSockets(t *testing.T) {
	var got []int
	add := func(p int) { got = append(got, p) }
	done, err := parseDiagDump(concat(
		nlmsg(sockDiagByFamily, diagMsg(tcpListen, 30000)),
		nlmsg(sockDiagByFamily, diagMsg(1, 30001)),              // ESTABLISHED
		nlmsg(sockDiagByFamily, diagMsg(6, 30002)),              // TIME_WAIT
		nlmsg(sockDiagByFamily, diagMsg(tcpListen, 30003)[:40]), // short payload
		nlmsg(sockDiagByFamily, diagMsg(tcpListen, 30004)),
	), add)
	if done || err != nil {
		t.Fatalf("first datagram: done=%v err=%v, want more to come", done, err)
	}
	done, err = parseDiagDump(nlmsg(nlmsgDone, make([]byte, 4)), add)
	if !done || err != nil {
		t.Fatalf("done datagram: done=%v err=%v, want done", done, err)
	}
	if !reflect.DeepEqual(got, []int{30000, 30004}) {
		t.Fatalf("ports = %v, want [30000 30004]", got)
	}
}

func TestParseDiagDumpEndsOnKernelError(t *testing.T) {
	done, err := parseDiagDump(errMsg(syscall.EAFNOSUPPORT), func(int) {})
	if !done || !errors.Is(err, syscall.EAFNOSUPPORT) {
		t.Fatalf("done=%v err=%v, want done with EAFNOSUPPORT", done, err)
	}
	if done, err := parseDiagDump(errMsg(0), func(int) {}); !done || err != nil {
		t.Fatalf("ack: done=%v err=%v, want done without error", done, err)
	}
}

func TestParseDiagDumpRejectsTruncatedMessages(t *testing.T) {
	whole := nlmsg(sockDiagByFamily, diagMsg(tcpListen, 30000))
	for name, b := range map[string][]byte{
		"length past datagram": whole[:len(whole)-8],
		"length below header":  append([]byte{4, 0, 0, 0}, whole[4:]...),
		"error without code":   nlmsg(nlmsgError, nil),
	} {
		if done, err := parseDiagDump(b, func(int) {}); !done || !errors.Is(err, errTruncated) {
			t.Errorf("%s: done=%v err=%v, want errTruncated", name, done, err)
		}
	}
}

func TestListenPortsInFiltersToRangeAndKeepsPartialResults(t *testing.T) {
	failure := errors.New("ipv6 refused")
	got, err := listenPortsIn(proto.PortRange{Start: 30000, End: 30010}, func() ([]int, error) {
		return []int{22, 30005, 30000, 30005, 30010, 30011, 8080}, failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the dump error passed through", err)
	}
	if !reflect.DeepEqual(got, []int{30000, 30005, 30010}) {
		t.Fatalf("ports = %v, want [30000 30005 30010]", got)
	}
	got, err = listenPortsIn(proto.PortRange{Start: 30000, End: 30010}, func() ([]int, error) { return nil, nil })
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no listeners: ports=%#v err=%v, want empty non-nil", got, err)
	}
}
