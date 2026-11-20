package store

import (
	"sync"
)

type ClusterSessionPeer struct {
	ID      uint64
	Address string
}

type ClusterSessionRoute struct {
	ID     uint64
	Key    []byte
	Secure bool // Native clusters require certificate-bound member transport.
	Listen string
	Peers  []ClusterSessionPeer
}

var clusterSessionRoutes sync.Map

// NoteClusterSessionRoute records the member identity a replicated database
// open observed. An empty identity is ignored.
func (s *Store) NoteClusterSessionRoute(route ClusterSessionRoute) {
	if s == nil || route.ID == 0 || (!route.Secure && len(route.Key) == 0) {
		return
	}
	copied := route
	copied.Key = append([]byte(nil), route.Key...)
	copied.Peers = append([]ClusterSessionPeer(nil), route.Peers...)
	clusterSessionRoutes.Store(s, &copied)
}

// ClusterSessionRoute reports the membership noted for this process.
func (s *Store) ClusterSessionRoute() (ClusterSessionRoute, bool) {
	if s == nil {
		return ClusterSessionRoute{}, false
	}
	value, ok := clusterSessionRoutes.Load(s)
	if !ok {
		return ClusterSessionRoute{}, false
	}
	route, ok := value.(*ClusterSessionRoute)
	if !ok || route == nil || route.ID == 0 || (!route.Secure && len(route.Key) == 0) {
		return ClusterSessionRoute{}, false
	}
	out := *route
	out.Key = append([]byte(nil), route.Key...)
	out.Peers = append([]ClusterSessionPeer(nil), route.Peers...)
	return out, true
}
