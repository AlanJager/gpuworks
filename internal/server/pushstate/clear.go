package pushstate

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// NotARestriction refuses clearing a table of kind, which is not a
// restriction: clearing a grant would take away what its owner granted
// rather than release what it restricted, so only an owner's own push or
// its uninstall removes one.
func NotARestriction(owner, kind string) *messages.ProductError {
	return messages.NewProductError(messages.ErrorBadRequest, "only a restriction table ("+restrictKindList()+") can be cleared, not "+kind,
		map[string]any{"plugin": owner, "kind": kind})
}

func restrictKindList() string {
	var out string
	for _, k := range extapi.TableKinds() {
		if extapi.ClassOf(k) != extapi.Restrict {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += k
	}
	return out
}

// ClearTable removes owner's table of kind, a restriction, and every
// override of it, in one commit, as if the owner had never pushed it: the
// holds it set are released in one pass of the scheduling holds, and the
// owner's next push of the kind starts a fresh table at any generation.
// It is the offline `gpuworks plugin pstate clear`, applied at boot from
// the maintenance journal before any plugin starts. Clearing a table that
// is not there commits nothing new and succeeds, so applying the journal
// twice is harmless. A grant kind is refused with NotARestriction; a
// persistence failure is returned as it is and changes nothing.
func (s *State) ClearTable(ctx context.Context, owner, kind string) error {
	if extapi.ClassOf(kind) != extapi.Restrict {
		return NotARestriction(owner, kind)
	}
	k := ownerKind{owner, kind}
	return s.enqueue(ctx, &op{
		clear: &pstate.OwnerKind{Owner: owner, Kind: kind},
		build: func(*pstate.Batch) error { return nil },
		after: func(_ pstate.Result, err error) {
			if err != nil {
				return
			}
			delete(s.parts, k)
			for id, o := range s.overrides {
				if o.Owner == owner && o.Kind == kind {
					delete(s.overrides, id)
				}
			}
			for id, so := range s.superseded {
				if so.o.Owner == owner && so.o.Kind == kind {
					delete(s.superseded, id)
				}
			}
		},
	})
}
