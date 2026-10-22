package proxy

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// Serving traffic is counted on the connection the gateway opens to a node,
// the one place both paths (the node's serving network and the agent tunnel)
// pass through. It is reported from the node's side, like the agent's own
// storage-sync counters: what the gateway sends is the node's rx.
const (
	metricRoleRxBytes = "gpuworks_node_role_rx_bytes_per_sec"
	metricRoleTxBytes = "gpuworks_node_role_tx_bytes_per_sec"
	roleServing       = "serving"
)

type nodeBytes struct{ rx, tx atomic.Uint64 }

// servingTraffic accumulates bytes per node and turns them into rates at
// each accounting flush. Nodes are kept once seen so a node that stops
// serving reports zero rather than vanishing mid-chart.
type servingTraffic struct {
	mu     sync.Mutex
	nodes  map[string]*nodeBytes
	last   map[string][2]uint64
	lastAt time.Time
}

func newServingTraffic() *servingTraffic {
	return &servingTraffic{nodes: map[string]*nodeBytes{}, last: map[string][2]uint64{}}
}

func (s *servingTraffic) node(uuid string) *nodeBytes {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[uuid]
	if !ok {
		n = &nodeBytes{}
		s.nodes[uuid] = n
	}
	return n
}

// samples reports each node's rates since the previous call; the first call
// only sets the baseline.
func (s *servingTraffic) samples(now time.Time) []tsdb.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevAt := s.lastAt
	s.lastAt = now
	dt := now.Sub(prevAt).Seconds()
	var out []tsdb.Sample
	for uuid, n := range s.nodes {
		cur := [2]uint64{n.rx.Load(), n.tx.Load()}
		prev := s.last[uuid] // zero for a node first seen in this interval
		s.last[uuid] = cur
		if prevAt.IsZero() || dt <= 0 {
			continue
		}
		labels := map[string]string{"node": uuid, "role": roleServing}
		out = append(out,
			tsdb.Sample{Metric: metricRoleRxBytes, Labels: labels, Value: float64(cur[0]-prev[0]) / dt, TimeMS: now.UnixMilli()},
			tsdb.Sample{Metric: metricRoleTxBytes, Labels: labels, Value: float64(cur[1]-prev[1]) / dt, TimeMS: now.UnixMilli()},
		)
	}
	return out
}

// countedConn counts the bytes of one gateway-to-node connection.
type countedConn struct {
	net.Conn
	bytes *nodeBytes
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.bytes.tx.Add(uint64(n))
	return n, err
}

func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.bytes.rx.Add(uint64(n))
	return n, err
}

// countServing wraps a connection to a node so its bytes are reported as
// that node's serving traffic. A nil Accountant counts nothing.
func (a *Accountant) countServing(conn net.Conn, nodeUUID string) net.Conn {
	if a == nil || conn == nil || nodeUUID == "" {
		return conn
	}
	return &countedConn{Conn: conn, bytes: a.traffic.node(nodeUUID)}
}
