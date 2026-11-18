package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// CardLeaser borrows an instance's cards from an allocator outside the
// platform before the instance starts: on a Slurm node the cards belong to
// Slurm, and the platform uses one only while a job of its own holds it.
//
// Such an allocator takes a number of cards, not a choice of them, so the
// cards it hands out may not be the ones placement chose. Lease returns the
// cards the instance now holds (accelerator.CardIdent, as many as want), and
// whether they were taken just now: cards already held for the instance
// (an agent restart adopting its running container) are the ones its
// container runs on, newly taken ones may not be.
//
// shared says the instance shares its cards with others (dGPU, a memory
// limit per card); one that takes whole cards never runs on cards held for
// another instance.
//
// The leaser returns cards by itself once no instance holds them
// (InstanceCards), so there is no release call to forget.
type CardLeaser interface {
	Lease(ctx context.Context, instance string, want []string, shared bool, progress func(message string)) (held []string, fresh bool, err error)
}

// SetCardLeaser installs the node's card allocator. canRebind reports whether
// the connected server rebinds an instance to cards it did not place it on
// (proto.CapInstanceCardRebind); without it, cards other than the placed ones
// fail the start.
func (e *Engine) SetCardLeaser(leaser CardLeaser, canRebind func() bool) {
	e.mu.Lock()
	e.cardLeaser = leaser
	e.canRebind = canRebind
	e.mu.Unlock()
}

// InstanceCards are the cards each instance this agent runs or is still
// tearing down holds, by instance UUID: what a leaser must not return.
func (e *Engine) InstanceCards() map[string][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string][]string{}
	for uuid, s := range e.desired {
		if s.DesiredState == "running" && len(s.GPUIdents) > 0 {
			out[uuid] = append(out[uuid], s.GPUIdents...)
		}
	}
	for uuid, s := range e.cleanupRequired {
		out[uuid] = append(out[uuid], s.GPUIdents...)
	}
	return out
}

// leaseCards borrows the spec's cards and, when the allocator gave others,
// rebinds the spec to them. rebound says the container, if one runs, was
// started on cards the instance no longer holds.
func (e *Engine) leaseCards(ctx context.Context, spec *proto.InstanceDesiredState) (rebound bool, err error) {
	e.mu.Lock()
	leaser, canRebind := e.cardLeaser, e.canRebind
	e.mu.Unlock()
	if leaser == nil || len(spec.GPUIdents) == 0 {
		return false, nil
	}
	if len(spec.GPUIdents) != len(spec.GPUIndexes) {
		err := fmt.Errorf("the instance's cards are not all identified, so they cannot be borrowed from the node's allocator")
		return false, withFailure(proto.RuntimeFailure{Code: proto.ErrorCardLeaseFailed, Detail: err.Error()}, err)
	}
	held, fresh, err := leaser.Lease(ctx, spec.UUID, spec.GPUIdents, spec.VRAMLimitBytes > 0, func(message string) {
		e.reportStateProgress(*spec, proto.InstPending, "", "", message, proto.PhaseContainerPrepare, 0, 0, 0, 0)
	})
	if err != nil {
		return false, withFailure(proto.RuntimeFailure{Code: proto.ErrorCardLeaseFailed, Detail: err.Error()}, err)
	}
	if slices.Equal(held, spec.GPUIdents) {
		return false, nil
	}
	if sameCards(held, spec.GPUIdents) {
		// The same cards in another order: keep the placed order, which the
		// device indexes and any tensor-parallel layout follow.
		return false, nil
	}
	if canRebind == nil || !canRebind() {
		err := fmt.Errorf("the node's allocator gave cards %s instead of the placed %s, and the server cannot rebind the instance (upgrade the server)",
			strings.Join(held, ", "), strings.Join(spec.GPUIdents, ", "))
		return false, withFailure(proto.RuntimeFailure{Code: proto.ErrorCardLeaseRebindUnsupported, Resource: strings.Join(held, ", ")}, err)
	}
	indexes, err := e.rebindIndexes(ctx, spec.GPUIdents, held)
	if err != nil {
		return false, err
	}
	placed := spec.GPUIdents
	spec.GPUIdents, spec.GPUIndexes = held, indexes
	e.mu.Lock()
	// The desired entry is rebound too, so the cards are counted as this
	// agent's (HeldCards) before the server's copy catches up; a newer spec
	// that already names other cards is left to its own start.
	if current, ok := e.desired[spec.UUID]; ok && slices.Equal(current.GPUIdents, placed) {
		current.GPUIdents, current.GPUIndexes = held, indexes
		e.desired[spec.UUID] = current
	}
	e.mu.Unlock()
	e.report(proto.InstanceStatus{
		UUID: spec.UUID, State: proto.InstPending, Phase: proto.PhaseContainerPrepare,
		Message:   "the node's allocator gave cards " + strings.Join(held, ", "),
		GPUIdents: held,
	})
	return fresh, nil
}

// rebindIndexes is the device index of each held card, after checking every
// one is the same kind of card as the one placed in its position: placement
// sized the instance for those, and a smaller card would not fit it.
func (e *Engine) rebindIndexes(ctx context.Context, placed, held []string) ([]int, error) {
	if e.prober == nil {
		err := fmt.Errorf("the node's cards cannot be enumerated to rebind the instance")
		return nil, withFailure(proto.RuntimeFailure{Code: proto.ErrorCardLeaseFailed, Detail: err.Error()}, err)
	}
	gpus, err := e.prober.Probe(ctx)
	if err != nil {
		err = fmt.Errorf("probe the cards to rebind the instance: %w", err)
		return nil, withFailure(proto.RuntimeFailure{Code: proto.ErrorCardLeaseFailed, Detail: err.Error()}, err)
	}
	byIdent := map[string]proto.GPUInfo{}
	for _, g := range gpus {
		if g.Index < 0 {
			continue
		}
		if ident := accelerator.CardIdent(g.UUID, g.PCIeBDF); ident != "" {
			byIdent[ident] = g
		}
	}
	indexes := make([]int, 0, len(held))
	for i, ident := range held {
		got, ok := byIdent[ident]
		if !ok {
			err := fmt.Errorf("the node's allocator gave card %s, which this node does not list", ident)
			return nil, withFailure(proto.RuntimeFailure{Code: proto.ErrorCardLeaseFailed, Resource: ident, Detail: err.Error()}, err)
		}
		if want, ok := byIdent[placed[i]]; ok && (want.Name != got.Name || want.MemoryTotalBytes != got.MemoryTotalBytes) {
			detail := fmt.Sprintf("%s, %d MiB in place of %s (%s, %d MiB)", got.Name, got.MemoryTotalBytes>>20, placed[i], want.Name, want.MemoryTotalBytes>>20)
			err := fmt.Errorf("the node's allocator gave card %s (%s)", ident, detail)
			return nil, withFailure(proto.RuntimeFailure{Code: proto.ErrorCardLeaseCardMismatch, Resource: ident, Detail: detail}, err)
		}
		indexes = append(indexes, got.Index)
	}
	return indexes, nil
}

func sameCards(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// LeaseLost stops an instance whose cards the allocator took back (a Slurm
// job cancelled, timed out or preempted): its engine is on cards the
// platform no longer holds. It is reported failed, so the control plane
// places it again.
func (e *Engine) LeaseLost(ctx context.Context, uuid, message string) {
	e.mu.Lock()
	spec, ok := e.desired[uuid]
	epoch := e.runtimeEpoch[uuid]
	e.mu.Unlock()
	if !ok || spec.DesiredState != "running" {
		return
	}
	e.log.Warn("instance's cards were taken back by the node's allocator", "instance", uuid, "reason", message)
	e.retireStopped(ctx, spec, epoch, proto.RuntimeFailure{Code: proto.ErrorCardLeaseLost, Phase: proto.PhaseServing, Detail: message}, message)
}
