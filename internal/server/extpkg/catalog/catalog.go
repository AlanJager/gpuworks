// Package catalog imports publishers' package indexes offline and decides,
// for each package an index lists, which version this server installs: the
// newest one whose requires it satisfies, that is neither revoked nor below
// the package's floor; newer builds it cannot run are listed with
// PLUGIN_NEEDS_NEWER_CORE. The server decides, the console only shows the
// result (docs/reference/plugin-protocol.md, "包索引").
package catalog

import (
	"fmt"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Imported describes an applied index.
type Imported struct {
	Index *extapi.Index
	// Issuer is the serial sequence the index belongs to; Anchor certified
	// its signing key.
	Issuer, Anchor string
}

// open verifies an index envelope with the certificates of its publisher
// and returns it with the anchor of the key that signed it.
func open(set *trust.Set, file, certs []byte) (*extapi.Index, string, error) {
	env, err := extapi.DecodeEnvelope(file)
	if err != nil {
		return nil, "", err
	}
	// The publisher picks which certificates may verify; it is read from
	// the unverified payload for that only, and decoded again verified.
	claimed, err := extapi.DecodeIndex(env.Payload)
	if err != nil {
		return nil, "", err
	}
	keys, anchors, err := set.PublisherKeys(claimed.Publisher, certs)
	if err != nil {
		return nil, "", err
	}
	x, keyIDs, err := extapi.OpenIndex(env, keys)
	if err != nil {
		return nil, "", err
	}
	return x, anchors[keyIDs[0]], nil
}

// Import verifies an index (an envelope) with its publisher's
// certificates, applies the index's revocations and floors through db, and
// saves both files in d for the catalogue. An index whose serial is not
// above the highest applied from its publisher is refused
// (ErrorPluginTrustReplayed) and changes nothing.
func Import(db trust.Store, set *trust.Set, d layout.Dir, file, certs []byte) (*Imported, error) {
	x, anchor, err := open(set, file, certs)
	if err != nil {
		return nil, err
	}
	in := &Imported{Index: x, Issuer: "index:" + anchor + ":" + x.Publisher, Anchor: anchor}
	applied, highest, err := db.ApplyPluginTrust(in.Issuer, anchor, extapi.TrustUpdate{
		Format: extapi.TrustUpdateFormat, Serial: x.Serial, Created: x.Created, Revoke: x.Revoked, Floors: x.Floors,
	})
	if err != nil {
		return nil, fmt.Errorf("apply index of %s: %w", x.Publisher, err)
	}
	if !applied {
		return nil, messages.NewProductError(messages.ErrorPluginTrustReplayed,
			fmt.Sprintf("index of %s serial %d is not newer than %d", x.Publisher, x.Serial, highest),
			map[string]any{"issuer": in.Issuer, "serial": x.Serial, "highest": highest})
	}
	if err := d.SaveIndex(x.Publisher, file, certs); err != nil {
		return nil, err
	}
	return in, nil
}

// Load verifies every saved index again against set. An index that no
// longer verifies (its publisher's issuer was removed from server.yaml, its
// key revoked from trust) is reported in errs and left out.
func Load(set *trust.Set, d layout.Dir) (indexes []*extapi.Index, errs map[string]error, err error) {
	saved, err := d.Indexes()
	if err != nil {
		return nil, nil, err
	}
	errs = map[string]error{}
	for _, s := range saved {
		x, _, err := open(set, s.Index, s.Certs)
		if err != nil {
			errs[s.Publisher] = err
			continue
		}
		indexes = append(indexes, x)
	}
	return indexes, errs, nil
}

// Choice is the decision for one package of an index.
type Choice struct {
	ID string
	// Best is the newest build this server can install, or nil.
	Best *extapi.IndexEntry
	// Newer are the builds newer than Best this server refuses, newest
	// first, each with the coded reason.
	Newer []Refused
}

// Refused is a build the server will not install, and why.
type Refused struct {
	Entry *extapi.IndexEntry
	Err   error
}

// Choose decides, per package of x, which build this server installs.
func Choose(x *extapi.Index, offered map[string]int, set *trust.Set) []Choice {
	byID := map[string][]*extapi.IndexEntry{}
	var ids []string
	for i := range x.Packages {
		e := &x.Packages[i]
		if _, ok := byID[e.ID]; !ok {
			ids = append(ids, e.ID)
		}
		byID[e.ID] = append(byID[e.ID], e)
	}
	sort.Strings(ids)
	out := make([]Choice, 0, len(ids))
	for _, id := range ids {
		c := Choice{ID: id}
		builds := byID[id]
		// Index entries are sorted by version precedence; newest last.
		for i := len(builds) - 1; i >= 0; i-- {
			e := builds[i]
			if err := refusal(x.Publisher, e, offered, set); err != nil {
				c.Newer = append(c.Newer, Refused{Entry: e, Err: err})
				continue
			}
			c.Best = e
			break
		}
		out = append(out, c)
	}
	return out
}

func refusal(publisher string, e *extapi.IndexEntry, offered map[string]int, set *trust.Set) error {
	if !e.Protocol.Contains(extapi.ProtocolMajor) {
		return &extapi.Error{Code: extapi.ErrorProtocolUnsupported,
			Params:  map[string]any{"min": e.Protocol.Min, "max": e.Protocol.Max, "supported": extapi.ProtocolMajor},
			Message: fmt.Sprintf("%s %s speaks gwx/%d..%d", e.ID, e.Version, e.Protocol.Min, e.Protocol.Max)}
	}
	caps := make([]string, 0, len(e.Requires))
	for c := range e.Requires {
		caps = append(caps, c)
	}
	sort.Strings(caps)
	for _, c := range caps {
		if offered[c] < e.Requires[c] {
			return messages.NewProductError(messages.ErrorPluginNeedsNewerCore,
				fmt.Sprintf("%s %s needs a newer server (%s): it requires %s@%d", e.ID, e.Version, e.MinCoreHint, c, e.Requires[c]),
				map[string]any{"id": e.ID, "version": e.Version, "min_core_hint": e.MinCoreHint, "capability": c})
		}
	}
	if r := set.ManifestRevocation(publisher, e.Manifest); r != nil {
		return messages.NewProductError(messages.ErrorPluginPackageRevoked, fmt.Sprintf("%s %s is revoked", e.ID, e.Version),
			map[string]any{"id": e.ID, "version": e.Version, "replacement": r.Replacement})
	}
	if floor := set.Floor(e.ID); floor != "" {
		if c, err := extapi.CompareVersions(e.Version, floor); err != nil || c < 0 {
			return messages.NewProductError(messages.ErrorPluginBelowFloor, fmt.Sprintf("%s %s is below the floor %s", e.ID, e.Version, floor),
				map[string]any{"id": e.ID, "version": e.Version, "floor": floor})
		}
	}
	return nil
}
