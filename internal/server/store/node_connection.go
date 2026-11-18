package store

// ConnectionStateForSession is the connection a client sees when the server
// knows whether this node has an authenticated control session. A stored
// online row can survive a server restart; without a session it is reconnecting
// until the ordinary heartbeat expiry settles it as offline. This projection
// never changes the stored heartbeat or the state placement reads.
func (n *Node) ConnectionStateForSession(connected bool) string {
	if n == nil {
		return ""
	}
	if connected {
		return NodeStateOnline
	}
	if n.State == NodeStateOnline {
		return NodeStateConnecting
	}
	return n.ConnectionState()
}
