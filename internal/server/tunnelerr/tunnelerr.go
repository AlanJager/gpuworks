// Package tunnelerr names the failures of a tunnel to a node's agent that
// both ends of it must tell apart: the hub, which opens the tunnel, and the
// inference gateway, which answers the client. It is a leaf: neither end
// has to import the other to recognise the error.
package tunnelerr

import "errors"

// ErrDataLaneUnavailable is returned by a data-plane stream open when the
// node's data lane is detached and does not reattach within the hub's wait,
// and by a read or write on an open stream whose lane died under it. The
// node is online and a retry will likely succeed.
var ErrDataLaneUnavailable = errors.New("data lane unavailable")
