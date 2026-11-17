package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// MaxNodeReadBatch is the most nodes one NodeIdentity call names and one
// NodeInventory page holds: both are pinned reads sized for a page of a
// console table, never a whole 1000-node fleet in one go.
const MaxNodeReadBatch = 500

// MaxIdentitySalt bounds NodeIdentity's salt.
const MaxIdentitySalt = 256

var (
	// ErrNodeReadBatch: a NodeIdentity call named no node or more than
	// MaxNodeReadBatch, or a NodeInventory limit outside 1..MaxNodeReadBatch.
	ErrNodeReadBatch = errors.New("node read: between 1 and 500 nodes per call")
	// ErrIdentitySalt: NodeIdentity's salt is empty or longer than
	// MaxIdentitySalt. An empty salt would make the digests the same for
	// every caller, which is what salting is there to prevent.
	ErrIdentitySalt = errors.New("node identity: the salt must be 1 to 256 bytes")
)

// IdentityPart is one salted hardware identity: H, the first 16 hex
// characters (8 bytes) of HMAC-SHA256 keyed with the caller's salt over
// kind, a zero byte and the raw value (kind "host" for the host system
// UUID, "card" for a card's serial); and Tail, the raw value's last four
// characters, so a person can check it against the machine. Tail is empty
// for a raw value shorter than eight characters, so it never is the whole
// value. The zero IdentityPart means the value is unknown.
type IdentityPart struct {
	H    string
	Tail string
}

// Known reports whether the part names a value.
func (p IdentityPart) Known() bool { return p.H != "" }

// NodeIdentity is what identifies one node's hardware, salted: the machine
// it runs on and the cards it owns. Raw serials never leave the server;
// two callers with different salts get unrelated digests of the same
// machine, one caller gets the same digests every time.
type NodeIdentity struct {
	NodeID uint
	// Host is the machine's system UUID (Node.HostSystemUUID); unknown
	// until the node's agent reports one. Logical nodes on one machine
	// share it.
	Host IdentityPart
	// Cards are the cards the node may use (accelerator.CardUsable) that
	// carry a hardware serial (accelerator.CardSerial), ordered by H.
	Cards []IdentityPart
	// CardsWithoutSerial counts the cards the node may use whose id the
	// probe made up from a slot or index (Ascend, and AMD or Hygon cards
	// without a unique id): they are counted, never digested.
	CardsWithoutSerial int
}

// NodeIdentity is the salted identity of each named node that exists, in
// ascending id order; a node that does not exist (never did, or was
// deleted) is left out, so the caller compares lengths or ids. Duplicate
// ids count once. It reads only the named nodes and their cards, two
// indexed queries on the read pool. ids holds 1 to MaxNodeReadBatch nodes
// (ErrNodeReadBatch) and salt 1 to MaxIdentitySalt bytes (ErrIdentitySalt).
func (s *Store) NodeIdentity(ctx context.Context, ids []uint, salt []byte) ([]NodeIdentity, error) {
	if len(salt) == 0 || len(salt) > MaxIdentitySalt {
		return nil, ErrIdentitySalt
	}
	ids = uniqueIDs(ids)
	if len(ids) == 0 || len(ids) > MaxNodeReadBatch {
		return nil, ErrNodeReadBatch
	}
	var hosts []nodeHostRow
	if err := s.reader().WithContext(ctx).Raw(nodeIdentityHostsSQL, ids).Scan(&hosts).Error; err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return []NodeIdentity{}, nil
	}
	cards, err := s.ownedCards(ctx, hostIDs(hosts))
	if err != nil {
		return nil, err
	}
	out := make([]NodeIdentity, len(hosts))
	for i, h := range hosts {
		id := NodeIdentity{NodeID: h.ID, Host: identityPart(salt, "host", h.HostSystemUUID), Cards: []IdentityPart{}}
		for _, c := range cards[h.ID] {
			serial, ok := accelerator.CardSerial(c.UUID)
			if !ok {
				id.CardsWithoutSerial++
				continue
			}
			id.Cards = append(id.Cards, identityPart(salt, "card", serial))
		}
		sort.Slice(id.Cards, func(a, b int) bool { return id.Cards[a].H < id.Cards[b].H })
		out[i] = id
	}
	return out, nil
}

// nodeHostRow is one node row NodeIdentity reads.
type nodeHostRow struct {
	ID             uint
	HostSystemUUID string
}

// nodeIdentityHostsSQL reads the named nodes by primary key.
const nodeIdentityHostsSQL = `SELECT id, host_system_uuid FROM nodes WHERE id IN ? ORDER BY id`

// ownedCardsSQL reads the cards the named nodes may use, through the
// (node_id, ident) index.
const ownedCardsSQL = `SELECT node_id, "index" AS card_index, name, uuid FROM gpus WHERE node_id IN ? AND ownership = ?`

// ownedCard is one card row ownedCards reads.
type ownedCard struct {
	NodeID    uint
	CardIndex int
	Name      string
	UUID      string
}

// ownedCards is the cards each named node may use, ordered by device index.
func (s *Store) ownedCards(ctx context.Context, nodeIDs []uint) (map[uint][]ownedCard, error) {
	var rows []ownedCard
	if err := s.reader().WithContext(ctx).Raw(ownedCardsSQL, nodeIDs, accelerator.CardOwnedByNode).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uint][]ownedCard, len(nodeIDs))
	for _, r := range rows {
		out[r.NodeID] = append(out[r.NodeID], r)
	}
	for _, cards := range out {
		sort.Slice(cards, func(a, b int) bool {
			if cards[a].CardIndex != cards[b].CardIndex {
				return cards[a].CardIndex < cards[b].CardIndex
			}
			return cards[a].UUID < cards[b].UUID
		})
	}
	return out, nil
}

func identityPart(salt []byte, kind, raw string) IdentityPart {
	if raw == "" {
		return IdentityPart{}
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(raw))
	p := IdentityPart{H: hex.EncodeToString(mac.Sum(nil)[:8])}
	if r := []rune(raw); len(r) >= 8 {
		p.Tail = string(r[len(r)-4:])
	}
	return p
}

func uniqueIDs(ids []uint) []uint {
	seen := make(map[uint]bool, len(ids))
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func hostIDs(hosts []nodeHostRow) []uint {
	out := make([]uint, len(hosts))
	for i, h := range hosts {
		out[i] = h.ID
	}
	return out
}
