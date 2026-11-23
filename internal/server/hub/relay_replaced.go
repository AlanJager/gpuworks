package hub

// A relayed agent connection (agent_relay.go) outlives its session here: the
// member holding it opens a new session whenever this one ends. That is right
// after a change of serving member and wrong after a replacement. When the
// agent connects again (its address moved to another entry host, a load
// balancer failed over) the connection it left may stay half open on the
// member that held it for as long as the keepalive allows; that member's
// relay and the new connection's would then replace each other's session
// without end, and so would two agent processes sharing a node.
//
// So the hub remembers, per node, the identity of the last session a
// different one replaced, and refuses that identity when it comes back. The
// refusal ends the relayed connection, as replacing a session ends a direct
// one. Only a relay presents an identity twice: a direct connection is given
// a fresh one. One identity per node, in memory; a member that starts
// serving knows none, and learns it at the first replacement.

// noteReplacedLocked records that s takes the node from old. Callers hold h.mu.
func (h *Hub) noteReplacedLocked(s, old *Session) {
	if old == nil || old.sessionID == "" || old.sessionID == s.sessionID {
		return
	}
	if h.replaced == nil {
		h.replaced = map[string]string{}
	}
	h.replaced[s.NodeUUID] = old.sessionID
}

// sessionReplaced reports whether sessionID is the identity of a session
// another one took the node from.
func (h *Hub) sessionReplaced(nodeUUID, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.replaced[nodeUUID] == sessionID
}
