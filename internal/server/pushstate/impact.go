package pushstate

import (
	"maps"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Impact is what uninstalling one owner would change, computed in memory
// by combining every part with the owner left out and comparing the result
// with what is enforced now (plugin-framework §7.6). An administrator
// confirms it by its Digest.
type Impact struct {
	Owner string
	// Version is the package version the caller named; it is digested too,
	// so an upgrade between the preview and the confirmation asks again.
	Version string
	// Tables are the owner's tables the uninstall deletes, by kind.
	Tables []ImpactTable
	// Nodes are the nodes whose hold the uninstall releases, ascending: the
	// owner's explicit holds no other owner also holds, and the nodes held
	// for capacity when no cap is left. When another owner's cap stays but
	// loosens, the nodes it would free are not counted here; Caps names
	// the loosened cap.
	Nodes []uint
	// Features are the feature keys that become locked, sorted.
	Features []string
	// Caps are the fleet caps lifted or loosened (EntryMaxNodesWithCards,
	// EntryMaxCards).
	Caps []string
	// Banners counts the owner's administrator banners shown now.
	Banners int
	// Overrides are the ids of the owner's overrides, which go with it.
	Overrides []int64
	// UsersAffected counts the users whose roles or mappings the owner
	// pushed; no table kind of schema 1 binds users, so it is 0.
	UsersAffected int
	// Digest is over everything above and each table's generation and
	// digest, so a push between the preview and the confirmation changes
	// it. An owner holding nothing has the digest of its id and version
	// alone, the same every time.
	Digest extapi.Digest
}

// ImpactTable is one table the uninstall deletes.
type ImpactTable struct {
	Kind       string        `json:"kind"`
	Entries    int           `json:"entries"`
	Generation uint64        `json:"generation"`
	Digest     extapi.Digest `json:"digest"`
}

// impactBody is what an Impact's digest is over.
type impactBody struct {
	Owner         string        `json:"owner"`
	Version       string        `json:"version"`
	Tables        []ImpactTable `json:"tables"`
	Nodes         []uint        `json:"nodes"`
	Features      []string      `json:"features"`
	Caps          []string      `json:"caps"`
	Banners       int           `json:"banners"`
	Overrides     []int64       `json:"overrides"`
	UsersAffected int           `json:"users_affected"`
}

// UninstallImpact is what uninstalling owner (at version) would change
// now. It reads memory only, never the database, and changes nothing: the
// recombination without the owner is thrown away. It answers for an
// uninstalled owner whose rows are still held (an uninstall that failed
// half way) as for an installed one.
func (s *State) UninstallImpact(owner, version string) (Impact, error) {
	s.mu.Lock()
	im := s.impactLocked(owner)
	s.mu.Unlock()
	im.Owner, im.Version = owner, version
	d, err := extapi.DigestValue(impactBody{Owner: owner, Version: version, Tables: nonNilList(im.Tables), Nodes: nonNilList(im.Nodes),
		Features: nonNilList(im.Features), Caps: nonNilList(im.Caps), Banners: im.Banners, Overrides: nonNilList(im.Overrides), UsersAffected: im.UsersAffected})
	if err != nil {
		return Impact{}, err
	}
	im.Digest = d
	return im, nil
}

func nonNilList[T any](l []T) []T {
	if l == nil {
		return []T{}
	}
	return l
}

func (s *State) impactLocked(owner string) Impact {
	var im Impact
	for _, kind := range extapi.TableKinds() {
		if p := s.parts[ownerKind{owner, kind}]; p != nil {
			im.Tables = append(im.Tables, ImpactTable{Kind: kind, Entries: EntriesOf(p.table), Generation: p.gen, Digest: p.digest})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.overrides)) {
		if s.overrides[id].Owner == owner {
			im.Overrides = append(im.Overrides, id)
		}
	}
	for _, b := range s.cur.Load().bannersAdmin {
		if bannerOf(b, owner) {
			im.Banners++
		}
	}

	now := s.clock.nowMS()
	ignore := func(int64) {}
	overrides := s.overridesLocked()
	withAvail := s.combineAvailabilityLocked(now, ignore)
	with := &Combined{}
	s.combineCapacityLocked(with, now, ignore, overrides)
	withHolds := s.combineHoldsLocked(overrides)

	// The same combination with owner left out of the installed packages,
	// as Uninstall leaves it; the registry is put back before the lock is
	// released, so no reader ever sees it.
	reg := s.reg
	s.reg = reg.without(owner)
	withoutAvail := s.combineAvailabilityLocked(now, ignore)
	without := &Combined{}
	s.combineCapacityLocked(without, now, ignore, overrides)
	withoutHolds := s.combineHoldsLocked(overrides)
	s.reg = reg

	released := map[uint]bool{}
	for id := range withHolds {
		if _, still := withoutHolds[id]; !still {
			released[id] = true
		}
	}
	if with.caps.Limited() && !without.caps.Limited() {
		if snap := s.cur.Load().capacity; snap != nil {
			for id, n := range snap.nodes {
				if n.held {
					if _, explicit := withoutHolds[id]; !explicit {
						released[id] = true
					}
				}
			}
		}
	}
	if len(released) > 0 {
		im.Nodes = slices.Sorted(maps.Keys(released))
	}
	if with.caps.NodesWithCards.Limited && without.caps.NodesWithCards != with.caps.NodesWithCards {
		im.Caps = append(im.Caps, EntryMaxNodesWithCards)
	}
	if with.caps.Cards.Limited && without.caps.Cards != with.caps.Cards {
		im.Caps = append(im.Caps, EntryMaxCards)
	}
	for _, k := range slices.Sorted(maps.Keys(withAvail)) {
		if withAvail[k].Access == featuregate.Locked {
			continue
		}
		if av, ok := withoutAvail[k]; !ok || av.Access == featuregate.Locked {
			im.Features = append(im.Features, string(k))
		}
	}
	return im
}

// bannerOf reports whether an administrator banner is owner's: one it
// pushed ("<owner>.<id>"), its down banner, or the notice that its push
// superseded an override.
func bannerOf(b Banner, owner string) bool {
	if strings.HasPrefix(b.ID, owner+".") || b.ID == "plugins.down."+owner {
		return true
	}
	if strings.HasPrefix(b.ID, "plugins.override_superseded.") && b.Notice != nil {
		return b.Notice.Params()["plugin"] == owner
	}
	return false
}

// without is r with package id left out, as SetPackages leaves it once id
// is uninstalled: the keys only it declared are declared by no one.
func (r *registry) without(id string) *registry {
	pkgs := make([]Package, 0, len(r.pkgs))
	for pid, p := range r.pkgs {
		if pid != id {
			pkgs = append(pkgs, p)
		}
	}
	return newRegistry(pkgs)
}
