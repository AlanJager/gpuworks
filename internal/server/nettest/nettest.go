// Package nettest runs a network's internal test. In ModeTCP every probed
// pair of nodes checks TCP reachability, RTT and throughput between their
// addresses on the network; in ModeRDMATool it runs ib_write_bw rail by rail
// (rdma.go); in the card modes each pair pings between the ports of its
// accelerator cards (cards.go). Pairs are every ordered pair (PatternMesh) or
// each node and the next one (PatternRing). It only orchestrates agent RPCs; which nodes,
// addresses and rails take part is decided by the caller.
package nettest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Endpoint is one node under test. IP is its address on the network (the
// bootstrap address on an RDMA network); an empty IP fails every pair it is in.
type Endpoint struct {
	NodeUUID string
	IP       string
	// Unavailable, a Code*, says why the node takes part in no pair
	// (CodeOffline, CodeDeleted).
	Unavailable string
	// Rails are the node's RDMA rails for ModeRDMATool.
	Rails []Rail
	// RDMAUnavailable, a Code*, says why the node cannot run ModeRDMATool
	// (CodeNoHCA, CodeToolMissing).
	RDMAUnavailable string
}

// Test modes.
const (
	ModeTCP      = "tcp"       // authenticated TCP RTT and throughput on the node addresses
	ModeRDMATool = "rdma_tool" // ib_write_bw, rail by rail
)

// Which pairs of nodes a run probes.
const (
	PatternMesh = "mesh" // every ordered pair: N*(N-1) pairs in N-1 rounds
	PatternRing = "ring" // each node probes the next, the last the first: N pairs in one round
)

// Why a pair or a rail failed. Side says which node the reason is about.
// Error on a Pair or RailResult is the English fallback and, for codes
// ending in _failed, the detail the client shows with the code.
const (
	CodeOffline         = "offline"
	CodeDeleted         = "deleted"
	CodeNoAddress       = "no_address" // the node resolved no address (RDMA: bootstrap) on the network
	CodeNoHCA           = "no_hca"     // the node uses no RDMA rail on the network
	CodeToolMissing     = proto.RDMABenchToolMissing
	CodeNoRoCEv2GID     = proto.RDMABenchNoRoCEv2GID
	CodeToolFailed      = proto.RDMABenchToolFailed
	CodeNoPeerRail      = "no_peer_rail" // the other node has no rail to pair this one with
	CodeListenerFailed  = "listener_failed"
	CodeRPCFailed       = "rpc_failed"
	CodeProbeFailed     = "probe_failed"
	CodeBandwidthFailed = "bandwidth_failed"
)

// Sides a failure code refers to.
const (
	SideSource = "source"
	SideTarget = "target"
)

// Pair is one directed probe from Source to Target.
type Pair struct {
	SourceUUID string  `json:"source_uuid"`
	TargetUUID string  `json:"target_uuid"`
	Target     string  `json:"target,omitempty"`
	Reachable  bool    `json:"reachable"`
	RTTP50MS   float64 `json:"rtt_ms_p50,omitempty"`
	RTTP99MS   float64 `json:"rtt_ms_p99,omitempty"`
	Gbps       float64 `json:"gbps,omitempty"`
	Code       string  `json:"code,omitempty"`
	Side       string  `json:"side,omitempty"`
	Error      string  `json:"error,omitempty"`
	// Rails holds per-rail results of ModeRDMATool; Gbps is their sum.
	Rails []RailResult `json:"rails,omitempty"`
}

// Pair outcomes.
const (
	StatusPassed  = "passed"
	StatusPartial = "partial" // some RDMA rails moved data, others failed
	StatusFailed  = "failed"
)

// Status is the pair's outcome: every rail (or the TCP probe, with
// bandwidth when measured) worked, only some rails did, or nothing did.
func (p Pair) Status(bandwidth bool) string {
	if p.Code != "" || (p.Error != "" && len(p.Rails) == 0) || !p.Reachable {
		return StatusFailed
	}
	if len(p.Rails) == 0 {
		if bandwidth && p.Gbps <= 0 {
			return StatusFailed
		}
		return StatusPassed
	}
	ok := 0
	for _, r := range p.Rails {
		if r.OK() {
			ok++
		}
	}
	switch ok {
	case len(p.Rails):
		return StatusPassed
	case 0:
		return StatusFailed
	}
	return StatusPartial
}

// Caller issues one agent RPC, as api.Deps.CallNode does.
type Caller func(ctx context.Context, nodeUUID, method string, params any) (json.RawMessage, error)

// Options tune a run.
type Options struct {
	Mode      string // ModeTCP (default), ModeRDMATool, ModeCardPing or ModeCardMatrix
	Pattern   string // PatternMesh (default) or PatternRing
	Bandwidth bool
	Seconds   int // per-pair bandwidth duration, 1..10
}

// Rounds is how many rounds a run of n nodes takes; each round runs one
// probe per node at once.
func Rounds(pattern string, n int) int {
	switch {
	case n < 2:
		return 0
	case pattern == PatternRing:
		return 1
	}
	return n - 1
}

// PairCount is how many pairs a run of n nodes probes.
func PairCount(pattern string, n int) int {
	return n * Rounds(pattern, n)
}

// Run probes the pattern's pairs and returns them sorted by source, target.
// In round k node i probes node i+k, so every node sends one stream and
// receives one stream at a time and a round takes as long as one probe
// regardless of the node count. The ring is round 1 alone; endpoints come in
// ring order.
func Run(ctx context.Context, call Caller, endpoints []Endpoint, opts Options) []Pair {
	if opts.Seconds <= 0 {
		opts.Seconds = 3
	}
	if CardMode(opts.Mode) {
		return runCards(ctx, call, endpoints, opts)
	}
	rounds := Rounds(opts.Pattern, len(endpoints))
	if opts.Mode == ModeRDMATool {
		return runRounds(endpoints, rounds, func(k int, source, target Endpoint) Pair {
			return rdmaProbe(ctx, call, source, target, rdmaBasePort+32*(k-1), opts.Seconds)
		})
	}
	listeners := startListeners(ctx, call, endpoints, rounds, opts)
	defer stopListeners(call, listeners)
	return runRounds(endpoints, rounds, func(_ int, source, target Endpoint) Pair {
		return probe(ctx, call, source, target, listeners[target.NodeUUID], opts)
	})
}

// runRounds probes the pairs of rounds 1..rounds and sorts the result.
func runRounds(endpoints []Endpoint, rounds int, pairFn func(round int, source, target Endpoint) Pair) []Pair {
	n := len(endpoints)
	pairs := make([]Pair, 0, n*rounds)
	var mu sync.Mutex
	for k := 1; k <= rounds; k++ {
		var wg sync.WaitGroup
		for i := range endpoints {
			source, target := endpoints[i], endpoints[(i+k)%n]
			wg.Add(1)
			go func() {
				defer wg.Done()
				pair := pairFn(k, source, target)
				mu.Lock()
				pairs = append(pairs, pair)
				mu.Unlock()
			}()
		}
		wg.Wait()
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].SourceUUID != pairs[j].SourceUUID {
			return pairs[i].SourceUUID < pairs[j].SourceUUID
		}
		return pairs[i].TargetUUID < pairs[j].TargetUUID
	})
	return pairs
}

// fail marks a pair failed for a reason about one side.
func (p *Pair) fail(code, side, detail string) {
	p.Code, p.Side, p.Error = code, side, fallback(code, side, detail)
}

// fallback is the English text of a code, for older clients.
func fallback(code, side, detail string) string {
	subject := side
	if subject == "" {
		subject = "node"
	}
	var msg string
	switch code {
	case CodeOffline:
		msg = subject + " is offline"
	case CodeDeleted:
		msg = subject + " was deleted"
	case CodeNoAddress:
		msg = subject + " has no address on this network"
	case CodeNoHCA:
		msg = subject + " has no active RDMA ports on this network"
	case CodeToolMissing:
		msg = subject + " has no ib_write_bw (install perftest)"
	case CodeUnreachable:
		return "no answer within the time limit"
	case CodeNoPeerRail:
		msg = "no " + map[string]string{SideSource: "target", SideTarget: "source"}[side] + " rail to pair with"
	default:
		msg = detail
		if side != "" && detail != "" {
			msg = side + ": " + detail
		}
		return msg
	}
	if detail != "" && code != CodeToolMissing {
		msg += ": " + detail
	}
	return msg
}

// unavailable fails a pair whose source or target cannot take part.
func (p *Pair) unavailable(source, target Endpoint) bool {
	switch {
	case source.Unavailable != "":
		p.fail(source.Unavailable, SideSource, "")
	case target.Unavailable != "":
		p.fail(target.Unavailable, SideTarget, "")
	default:
		return false
	}
	return true
}

type listener struct {
	token  string
	target string
	err    string
}

func startListeners(ctx context.Context, call Caller, endpoints []Endpoint, rounds int, opts Options) map[string]listener {
	ttl := rounds*(opts.Seconds+10) + 30
	if ttl > 3600 {
		ttl = 3600
	}
	out := make(map[string]listener, len(endpoints))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ep := range endpoints {
		if ep.Unavailable != "" || ep.IP == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := listener{}
			token, err := randomToken()
			if err == nil {
				l.token = token
				rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				var raw json.RawMessage
				raw, err = call(rctx, ep.NodeUUID, proto.RPCNetBenchStart, map[string]any{"port": 0, "token": token, "ttl_seconds": ttl})
				cancel()
				if err == nil {
					var started struct {
						Port int `json:"port"`
					}
					if err = json.Unmarshal(raw, &started); err == nil && started.Port <= 0 {
						err = fmt.Errorf("listener reported port %d", started.Port)
					}
					l.target = net.JoinHostPort(ep.IP, strconv.Itoa(started.Port))
				}
			}
			if err != nil {
				l.err = err.Error()
			}
			mu.Lock()
			out[ep.NodeUUID] = l
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func stopListeners(call Caller, listeners map[string]listener) {
	for uuid, l := range listeners {
		if l.token == "" || l.err != "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = call(ctx, uuid, proto.RPCNetBenchStop, map[string]string{"token": l.token})
		cancel()
	}
}

func probe(ctx context.Context, call Caller, source, target Endpoint, l listener, opts Options) Pair {
	pair := Pair{SourceUUID: source.NodeUUID, TargetUUID: target.NodeUUID, Target: l.target}
	switch {
	case pair.unavailable(source, target):
		return pair
	case source.IP == "":
		pair.fail(CodeNoAddress, SideSource, "")
		return pair
	case target.IP == "":
		pair.fail(CodeNoAddress, SideTarget, "")
		return pair
	case l.err != "":
		pair.fail(CodeListenerFailed, SideTarget, l.err)
		return pair
	}
	rctx, cancel := context.WithTimeout(ctx, time.Duration(opts.Seconds+20)*time.Second)
	defer cancel()
	raw, err := call(rctx, source.NodeUUID, proto.RPCNetProbe, map[string]any{
		"target": l.target, "token": l.token, "bandwidth": opts.Bandwidth, "seconds": opts.Seconds, "source_ip": source.IP,
	})
	if err != nil {
		pair.fail(CodeRPCFailed, SideSource, err.Error())
		return pair
	}
	var res struct {
		Reachable      bool    `json:"reachable"`
		P50            float64 `json:"rtt_ms_p50"`
		P99            float64 `json:"rtt_ms_p99"`
		Gbps           float64 `json:"gbps"`
		Error          string  `json:"error"`
		BandwidthError string  `json:"bandwidth_error"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		pair.fail(CodeRPCFailed, SideSource, "decode probe response: "+err.Error())
		return pair
	}
	pair.Reachable, pair.RTTP50MS, pair.RTTP99MS, pair.Gbps = res.Reachable, res.P50, res.P99, res.Gbps
	switch {
	case res.Error != "":
		pair.fail(CodeProbeFailed, "", res.Error)
	case res.BandwidthError != "":
		pair.fail(CodeBandwidthFailed, "", res.BandwidthError)
	}
	return pair
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
