package nettest

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// rdmaBasePort is the first ib_write_bw port; each round moves past the
// previous one's ports so a server nobody connected to cannot collide.
const rdmaBasePort = 18515

// Rail is one RDMA port of an endpoint: its name ("device:port") and its
// interface's IPv4 address with prefix ("" without one).
type Rail struct {
	Name string
	Addr string
}

func (r Rail) ip() string {
	ip, _, _ := strings.Cut(r.Addr, "/")
	return ip
}

// RailResult is one rail pair of a pair of nodes: the source rail (Rail,
// named "rail" on the wire as runs stored before rails were paired by
// subnet), the target rail it drove, and the outcome. ModeRDMATool measures
// Gbps; the card modes only whether the target answered (Reachable) and how
// fast (RTTMS).
type RailResult struct {
	Rail       string  `json:"rail"`
	SourceIP   string  `json:"source_ip,omitempty"`
	TargetRail string  `json:"target_rail,omitempty"`
	TargetIP   string  `json:"target_ip,omitempty"`
	Gbps       float64 `json:"gbps"`
	Reachable  bool    `json:"reachable,omitempty"`
	RTTMS      float64 `json:"rtt_ms,omitempty"`
	Code       string  `json:"code,omitempty"`
	Side       string  `json:"side,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// OK reports whether the rail moved data, or in a card mode answered.
func (r RailResult) OK() bool { return r.Code == "" && r.Error == "" && (r.Gbps > 0 || r.Reachable) }

// pairRails matches each source rail with a target rail. Rails whose
// addresses share a subnet pair first: a fabric wired as point-to-point
// links (a /30 or /31 per link) only connects the two ends of each link.
// The rest pair in order, as InfiniBand rails and one flat RoCE subnet do.
// Rails left over on the side with more have no peer.
func pairRails(source, target []Rail) (pairs [][2]Rail, lonelySource, lonelyTarget []Rail) {
	used := make([]bool, len(target))
	matched := make([]int, len(source))
	for i, s := range source {
		matched[i] = -1
		for j, t := range target {
			if !used[j] && sameSubnet(s.Addr, t.Addr) {
				matched[i], used[j] = j, true
				break
			}
		}
	}
	next := 0
	for i, s := range source {
		j := matched[i]
		if j < 0 {
			for next < len(target) && used[next] {
				next++
			}
			if next == len(target) {
				lonelySource = append(lonelySource, s)
				continue
			}
			j, used[next] = next, true
		}
		pairs = append(pairs, [2]Rail{s, target[j]})
	}
	for j, t := range target {
		if !used[j] {
			lonelyTarget = append(lonelyTarget, t)
		}
	}
	return pairs, lonelySource, lonelyTarget
}

// sameSubnet reports whether two addresses with prefixes lie in each other's
// subnet.
func sameSubnet(a, b string) bool {
	ipA, netA, errA := net.ParseCIDR(a)
	ipB, netB, errB := net.ParseCIDR(b)
	return errA == nil && errB == nil && netA.Contains(ipB) && netB.Contains(ipA)
}

// rdmaProbe serves ib_write_bw on the target's paired rails, then drives the
// source's rails at them. The handshake goes to the target's bootstrap
// address.
func rdmaProbe(ctx context.Context, call Caller, source, target Endpoint, basePort, seconds int) Pair {
	pair := Pair{SourceUUID: source.NodeUUID, TargetUUID: target.NodeUUID, Target: target.IP}
	switch {
	case pair.unavailable(source, target):
		return pair
	case source.RDMAUnavailable != "":
		pair.fail(source.RDMAUnavailable, SideSource, "")
		return pair
	case target.RDMAUnavailable != "":
		pair.fail(target.RDMAUnavailable, SideTarget, "")
		return pair
	case target.IP == "":
		pair.fail(CodeNoAddress, SideTarget, "")
		return pair
	}
	pairs, lonelySource, lonelyTarget := pairRails(source.Rails, target.Rails)
	sourceRails, targetRails := make([]string, len(pairs)), make([]string, len(pairs))
	for i, p := range pairs {
		sourceRails[i], targetRails[i] = p[0].Name, p[1].Name
	}

	serveCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	raw, err := call(serveCtx, target.NodeUUID, proto.RPCRDMABenchServe, proto.RDMABenchParams{Rails: targetRails, BasePort: basePort, Seconds: seconds})
	cancel()
	if err != nil {
		pair.fail(CodeRPCFailed, SideTarget, err.Error())
		return pair
	}
	var served proto.RDMABenchServed
	_ = json.Unmarshal(raw, &served) // older agents answer {"serving": true}
	unserved := map[string]proto.RDMABenchRail{}
	for _, r := range served.Failed {
		unserved[r.Rail] = r
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds+45)*time.Second)
	defer cancel()
	raw, err = call(runCtx, source.NodeUUID, proto.RPCRDMABenchRun, proto.RDMABenchParams{Rails: sourceRails, BasePort: basePort, Seconds: seconds, Target: target.IP})
	if err != nil {
		pair.fail(CodeRPCFailed, SideSource, err.Error())
		return pair
	}
	var ran []proto.RDMABenchRail
	if err := json.Unmarshal(raw, &ran); err != nil {
		pair.fail(CodeRPCFailed, SideSource, "decode rdma bench response: "+err.Error())
		return pair
	}
	pair.Reachable = true
	for i, p := range pairs {
		res := RailResult{Rail: p[0].Name, SourceIP: p[0].ip(), TargetRail: p[1].Name, TargetIP: p[1].ip()}
		switch failed, ok := unserved[p[1].Name]; {
		case ok:
			// The source only hit a closed port; the target's reason is the real one.
			res.Code, res.Side, res.Error = failed.Code, SideTarget, failed.Error
		case i < len(ran):
			res.Gbps, res.Code, res.Error = ran[i].Gbps, ran[i].Code, ran[i].Error
			if res.Code == CodeToolMissing || res.Code == CodeNoRoCEv2GID {
				res.Side = SideSource
			}
		default:
			res.Code, res.Side, res.Error = CodeRPCFailed, SideSource, "no result for this rail"
		}
		pair.Rails = append(pair.Rails, res)
		pair.Gbps += res.Gbps
	}
	for _, r := range lonelySource {
		pair.Rails = append(pair.Rails, RailResult{Rail: r.Name, SourceIP: r.ip(), Code: CodeNoPeerRail, Side: SideSource, Error: fallback(CodeNoPeerRail, SideSource, "")})
	}
	for _, r := range lonelyTarget {
		pair.Rails = append(pair.Rails, RailResult{TargetRail: r.Name, TargetIP: r.ip(), Code: CodeNoPeerRail, Side: SideTarget, Error: fallback(CodeNoPeerRail, SideTarget, "")})
	}
	return pair
}
