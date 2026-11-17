package trust

import (
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Kinds of trust file Import applies.
const (
	KindTrustUpdate = "trust_update"
	KindCRL         = "crl"
)

// Imported describes an applied trust file.
type Imported struct {
	// Kind is KindTrustUpdate or KindCRL.
	Kind string
	// Issuer names whose serial sequence the file belongs to: the trust
	// updates, the project roots' revocation lists, or one publisher's
	// lists under one anchor.
	Issuer string
	// Anchor is the anchor its revocations apply under.
	Anchor string
	Serial uint64
	// AddedRoots, RetiredRoots, Revocations and Floors count what it carried.
	AddedRoots, RetiredRoots, Revocations, Floors int
}

// Import verifies one trust file and applies it: a trust update signed by a
// root trusted now, a revocation list signed by a root (no publisher), or a
// publisher's own revocation list signed by a key one of certs (publisher.crt
// bytes) certifies. A file whose serial is not above the highest applied
// from its issuer is refused (ErrorPluginTrustReplayed) and changes nothing.
func Import(db Store, s *Set, file, certs []byte) (*Imported, error) {
	env, err := extapi.DecodeEnvelope(file)
	if err != nil {
		return nil, err
	}
	if isCRL(env.Payload) {
		return importCRL(db, s, env, certs)
	}
	u, err := extapi.OpenTrustUpdate(env, s.roots)
	if err != nil {
		return nil, err
	}
	for _, k := range u.AddRoots {
		if prev, ok := s.known(k.KeyID); ok && !sameKey(prev, k) {
			return nil, messages.NewProductError(messages.ErrorPluginTrustConflict,
				fmt.Sprintf("trust update adds root %s, already known as another key", k.KeyID),
				map[string]any{"keyid": k.KeyID})
		}
	}
	in := &Imported{Kind: KindTrustUpdate, Issuer: KindTrustUpdate, Anchor: RootAnchor, Serial: u.Serial,
		AddedRoots: len(u.AddRoots), RetiredRoots: len(u.RetireRoots), Revocations: len(u.Revoke), Floors: len(u.Floors)}
	return in, apply(db, in, *u)
}

// isCRL tells a revocation list from a trust update by its required
// "revoked" member; the payload is verified before anything of it is used.
func isCRL(payload []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(payload, &top) != nil {
		return false
	}
	_, ok := top["revoked"]
	return ok
}

func importCRL(db Store, s *Set, env *extapi.Envelope, certs []byte) (*Imported, error) {
	// Which keys may sign depends on the list's publisher, read here from
	// the unverified payload only to pick them; the list is decoded again
	// from the verified bytes.
	claimed, err := extapi.DecodeCRL(env.Payload)
	if err != nil {
		return nil, err
	}
	var in *Imported
	var verified *extapi.Verified
	if claimed.Publisher == "" {
		if verified, err = env.Verify("crl", s.roots); err != nil {
			return nil, err
		}
		in = &Imported{Kind: KindCRL, Issuer: "crl:" + RootAnchor, Anchor: RootAnchor}
	} else {
		keys, anchors, err := s.PublisherKeys(claimed.Publisher, certs)
		if err != nil {
			return nil, err
		}
		if verified, err = env.Verify("crl", keys); err != nil {
			return nil, err
		}
		anchor := anchors[verified.KeyIDs[0]]
		in = &Imported{Kind: KindCRL, Issuer: "crl:" + anchor + ":" + claimed.Publisher, Anchor: anchor}
	}
	list, err := extapi.DecodeCRL(verified.Payload)
	if err != nil {
		return nil, err
	}
	in.Serial, in.Revocations = list.Serial, len(list.Revoked)
	return in, apply(db, in, extapi.TrustUpdate{Format: extapi.TrustUpdateFormat, Serial: list.Serial, Created: list.Created, Revoke: list.Revoked})
}

// PublisherKeys opens the certificates of publisher in a publisher.crt and
// returns their keys and, per key id, the anchor that certified it.
func (s *Set) PublisherKeys(publisher string, certs []byte) (extapi.KeySet, map[string]string, error) {
	if len(certs) == 0 {
		return nil, nil, &extapi.Error{Code: extapi.ErrorSignatureUntrusted, Params: map[string]any{"object": "crl"},
			Message: "crl: a publisher's revocation list needs the publisher's certificate to verify"}
	}
	bundle, err := extapi.DecodeCertBundle(certs)
	if err != nil {
		return nil, nil, err
	}
	keys := extapi.KeySet{}
	anchors := map[string]string{}
	var firstErr error
	for i := range bundle {
		c, issuers, err := extapi.OpenPublisherCert(&bundle[i], s.issuers)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if c.Publisher != publisher {
			continue
		}
		keys[c.KeyID] = c.Key()
		anchors[c.KeyID] = s.Anchor(issuers)
	}
	if len(keys) == 0 {
		if firstErr != nil {
			return nil, nil, firstErr
		}
		return nil, nil, &extapi.Error{Code: extapi.ErrorSignatureUntrusted, Params: map[string]any{"object": "crl"},
			Message: "crl: no certificate of " + publisher + " in the bundle"}
	}
	return keys, anchors, nil
}

func apply(db Store, in *Imported, u extapi.TrustUpdate) error {
	applied, highest, err := db.ApplyPluginTrust(in.Issuer, in.Anchor, u)
	if err != nil {
		return fmt.Errorf("apply %s %d: %w", in.Kind, in.Serial, err)
	}
	if !applied {
		return messages.NewProductError(messages.ErrorPluginTrustReplayed,
			fmt.Sprintf("%s serial %d of %s is not newer than %d", in.Kind, in.Serial, in.Issuer, highest),
			map[string]any{"issuer": in.Issuer, "serial": in.Serial, "highest": highest})
	}
	return nil
}
