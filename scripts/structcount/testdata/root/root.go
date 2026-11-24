// Package root is a structcount fixture: every way of hiding a field, a lock,
// a map or an RPC case the ratchet must still see.
package root

import (
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
)

type uuidSet map[string]bool

type images struct {
	mu   sync.Mutex
	refs map[string]int
}

type Root struct {
	aMu, bMu sync.Mutex     // two locks on one line
	cMu      *sync.RWMutex  // a lock by pointer
	m1, m2   map[string]int // two maps on one line
	cache    sync.Map
	seen     uuidSet
	nested   struct { // wrapping does not hide them
		dMu  sync.Mutex
		byID map[string]int
	}
	images *images // a subsystem of its own counts once
	sync.Mutex
}

func (r *Root) OnRPC(method string) {
	switch method {
	case proto.RPCPing, proto.RPCDoctor:
	default:
		r.more(method)
	}
}

func (r Root) more(method string) {
	switch method {
	case proto.RPCPing: // counted once
	case proto.RPCSmoke:
	}
}

// A function that takes the root is a method by another spelling.
func helper(r *Root, method string) bool {
	return method == proto.RPCRestart // an RPC compared, not switched on
}

// A table of handlers on the root dispatches as a switch does.
var onRoot = map[string]func(*Root){
	proto.RPCUpgrade: (*Root).testOnlyTarget,
}

// A subsystem's own table and its own functions do not count.
var onImages = map[string]func(*images){
	proto.RPCPull: (*images).pull,
}

func newRoot() *Root { return &Root{} }

func (r *Root) testOnlyTarget() {}

func (i *images) pull() {}

// A method of another type that takes the root reaches it all the same.
func (i *images) route(r *Root, method string) bool {
	return method == proto.RPCRoute
}

// A table of closures built where the root is in scope.
func (r *Root) table() map[string]func() {
	return map[string]func(){proto.RPCClosure: func() { r.more("") }}
}

func (i *images) serve(method string) bool {
	switch method {
	case proto.RPCRemove:
	}
	return method == proto.RPCInspect
}
