package store

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// A scheduling holder is an extension that owns part of the fleet's
// availability: while it holds a node, the node is cordoned under the
// holder's source (Node.SchedulingBy) and takes no new replica; what runs
// there keeps running. Holds outrank the platform's own cordon and rank
// below an operator's: an operator may still cordon or drain a held node,
// but cannot make it schedulable while it is held (ErrNodeHeld).
//
// A hold is not stored: it is the holder's answer for the node's current
// facts, asked again on every status report and every scheduling change,
// so a node whose hold ends is released by the next one (or at once by
// ApplySchedulingHolds). Only its effect, the cordon and its source, is.

// NodeHoldFacts is what a holder decides on: the node's identity and the
// facts a status report keeps current.
type NodeHoldFacts struct {
	ID        uint
	UUID      string
	CreatedAt time.Time
	// OwnedCards is the cards the node may use (Node.GPUCount).
	OwnedCards    int
	GPUVendor     string
	KubeClusterID uint
}

// NodeHold is a holder's answer for one node. The zero value does not hold
// it. A hold is shown to an operator as a product message (ADR 0067), so
// it carries a code, its English fallback and the code's params; an empty
// code is shown as NODE_SCHEDULING_HELD.
type NodeHold struct {
	Code     string
	Fallback string
	Params   map[string]any
}

// Held reports whether h holds the node.
func (h NodeHold) Held() bool { return h.Code != "" || h.Fallback != "" || len(h.Params) > 0 }

// ProductError is the hold as an operator is told it.
func (h NodeHold) ProductError() *messages.ProductError {
	code, fallback := h.Code, h.Fallback
	if code == "" {
		code = messages.ErrorNodeSchedulingHeld
	}
	if fallback == "" {
		fallback = "the node is held out of scheduling"
	}
	return messages.NewProductError(code, fallback, h.Params)
}

// SchedulingHolder holds nodes out of scheduling.
type SchedulingHolder interface {
	// Source names the holder; it is what Node.SchedulingBy records for a
	// node it holds. Lower case letters, digits and _, at most 16, starting
	// with a letter, and neither "operator" nor "platform".
	Source() SchedulingSource
	// Hold answers for one node. It runs inside the writer's transactions
	// (every status report of every node), which is a single connection:
	// it must be pure, O(1) and allocation-free when it does not hold, and
	// never call the Store.
	Hold(f NodeHoldFacts) NodeHold
}

var (
	// ErrNodeHeld is wrapped, with the hold's *messages.ProductError, by
	// SetNodeScheduling asked to make a held node schedulable.
	ErrNodeHeld = errors.New("node is held out of scheduling")
	// ErrHolderConflict: a holder with the same source is registered.
	ErrHolderConflict = errors.New("a scheduling holder with this source is already registered")
)

var holderSourcePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,15}$`)

// AddSchedulingHolder registers h. Holders are asked in registration order
// and the first that holds a node owns it. A second holder with the same
// source is refused with ErrHolderConflict.
func (s *Store) AddSchedulingHolder(h SchedulingHolder) error {
	if h == nil {
		return errors.New("scheduling holder is nil")
	}
	src := h.Source()
	if !holderSourcePattern.MatchString(string(src)) || src == SchedulingByOperator || src == SchedulingByPlatform {
		return fmt.Errorf("scheduling holder source %q: want 1-16 of [a-z0-9_] starting with a letter, other than operator and platform", src)
	}
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	current := s.admission.schedulingHolders()
	for _, other := range current {
		if other.Source() == src {
			return fmt.Errorf("%w: %s", ErrHolderConflict, src)
		}
	}
	next := append(append([]SchedulingHolder(nil), current...), h)
	s.admission.holders.Store(&next)
	return nil
}

func (a *nodeAdmission) schedulingHolders() []SchedulingHolder {
	if p := a.holders.Load(); p != nil {
		return *p
	}
	return nil
}

// holdOf is the first registered holder's hold on the node, and its source;
// "" when none holds it.
func (a *nodeAdmission) holdOf(f NodeHoldFacts) (SchedulingSource, NodeHold) {
	for _, h := range a.schedulingHolders() {
		if hold := h.Hold(f); hold.Held() {
			return h.Source(), hold
		}
	}
	return SchedulingBySelf, NodeHold{}
}

// registeredSource reports whether src is a registered holder's.
func (a *nodeAdmission) registeredSource(src SchedulingSource) bool {
	for _, h := range a.schedulingHolders() {
		if h.Source() == src {
			return true
		}
	}
	return false
}

func nodeHoldFacts(n *Node) NodeHoldFacts {
	return NodeHoldFacts{ID: n.ID, UUID: n.UUID, CreatedAt: n.CreatedAt, OwnedCards: n.GPUCount, GPUVendor: n.GPUVendor, KubeClusterID: n.KubeClusterID}
}

// HoldOf is the registered holders' hold on n as its row stands; the zero
// NodeHold when none holds it. It reads nothing.
func (s *Store) HoldOf(n *Node) NodeHold {
	_, hold := s.admission.holdOf(nodeHoldFacts(n))
	return hold
}

// IsHolderSource reports whether src names a scheduling holder rather than
// nobody, an operator or the platform.
func IsHolderSource(src SchedulingSource) bool {
	return src != SchedulingBySelf && src != SchedulingByOperator && src != SchedulingByPlatform
}
