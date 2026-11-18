package extapi

import (
	"net/url"
	"regexp"
	"strconv"
	"unicode/utf8"
)

// Object names of the host API's node reads.
const (
	objectNodeIdentityRequest = "node_identity_request"
	objectNodeIdentityAnswer  = "node_identity_answer"
	objectNodesPage           = "nodes_page"
	objectNodesQuery          = "nodes_query"
)

// The node reads of the host API, frozen with protocol major 1. Each needs
// its permission granted and its capability negotiated: read:node_identity
// (CapHostNodeIdentity) and read:node_inventory (CapHostNodes).
const (
	// PathHostNodeIdentity answers POST with a NodeIdentityAnswer for a
	// NodeIdentityRequest. It is a POST so the salt never appears in a URL.
	PathHostNodeIdentity = "/host/v1/node-identity"
	// PathHostNodes answers GET ?cursor=&limit= with a NodesPage.
	PathHostNodes = "/host/v1/nodes"
)

// Bounds of the node reads.
const (
	// MaxNodeRead is the most nodes one identity request names and one
	// inventory page holds: a page of a console table, never a whole fleet.
	MaxNodeRead = 500
	// MaxNodeIdentitySalt bounds a plugin's salt, in bytes. The server keys
	// its digest with "pkg", a zero byte, the package id (at most 32
	// bytes), a zero byte and the salt, and that key is at most 256 bytes,
	// so two packages never get the same digests from the same salt.
	MaxNodeIdentitySalt = 219
	// MaxNodeCards bounds the cards of one node in either answer.
	MaxNodeCards = 256
	// MaxNodeText bounds a node's name and a card's model, in bytes.
	MaxNodeText = 255
	// maxNodeUUID bounds a node's UUID, in bytes.
	maxNodeUUID = 64
	// MaxNodeIdentityRequestBytes bounds an encoded identity request; the
	// server refuses a larger body as too large before decoding it.
	MaxNodeIdentityRequestBytes = 32 << 10
)

var (
	// An opaque page cursor; the server alone gives it meaning.
	cursorPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// An identity digest: 16 lowercase hex characters (8 bytes).
	identityDigestPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// NodeIdentityRequest is the body of POST /host/v1/node-identity.
type NodeIdentityRequest struct {
	// Salt keys the digests; the server binds it to the package, so the
	// same salt gives another package unrelated digests. 1 to
	// MaxNodeIdentitySalt bytes.
	Salt string `json:"salt"`
	// NodeIDs are the server's node ids, 1 to MaxNodeRead, without
	// repeats.
	NodeIDs []uint64 `json:"node_ids"`
}

// DecodeNodeIdentityRequest decodes and validates an identity request.
func DecodeNodeIdentityRequest(data []byte) (*NodeIdentityRequest, error) {
	if len(data) > MaxNodeIdentityRequestBytes {
		return nil, malformed(objectNodeIdentityRequest, &fieldError{rule: RuleRange})
	}
	var r NodeIdentityRequest
	if err := decodeStrict(objectNodeIdentityRequest, data, &r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks the request's rules.
func (r *NodeIdentityRequest) Validate() error {
	f := text("salt", r.Salt, MaxNodeIdentitySalt)
	if f == nil {
		f = nodeIDList("node_ids", r.NodeIDs)
	}
	if f != nil {
		return wireInvalid(objectNodeIdentityRequest, f)
	}
	return nil
}

func nodeIDList(path string, ids []uint64) *fieldError {
	if len(ids) == 0 {
		return &fieldError{field: path, rule: RuleRequired}
	}
	if len(ids) > MaxNodeRead {
		return &fieldError{field: path, rule: RuleRange}
	}
	seen := make(map[uint64]bool, len(ids))
	for i, id := range ids {
		at := joinPath(path, strconv.Itoa(i))
		if f := inRange(at, int64(min(id, maxSafeInteger+1)), 1, maxSafeInteger); f != nil {
			return f
		}
		if seen[id] {
			return &fieldError{field: at, rule: RuleDuplicate}
		}
		seen[id] = true
	}
	return nil
}

// IdentityPart is one salted hardware identity: H, the first 16 hex
// characters of an HMAC-SHA256 of the raw value, and Tail, the raw value's
// last four characters, unsalted, so a person can check it against the
// machine. Tail is empty when the raw value is shorter than eight
// characters.
type IdentityPart struct {
	H    string `json:"h"`
	Tail string `json:"tail,omitempty"`
}

func (p IdentityPart) validate(path string) *fieldError {
	f := matches(joinPath(path, "h"), p.H, identityDigestPattern)
	if f == nil && (len(p.Tail) > 16 || utf8.RuneCountInString(p.Tail) > 4) {
		f = &fieldError{field: joinPath(path, "tail"), rule: RuleRange}
	}
	return f
}

// NodeIdentity is the salted identity of one node: the machine it runs on
// (absent while its agent has not reported one) and the cards it may use
// that carry a hardware serial, ordered by H. Cards whose id the probe made
// up from a slot or an index are only counted.
type NodeIdentity struct {
	NodeID             uint64         `json:"node_id"`
	Host               *IdentityPart  `json:"host,omitempty"`
	Cards              []IdentityPart `json:"cards"`
	CardsWithoutSerial int            `json:"cards_without_serial"`
}

// NodeIdentityAnswer answers a NodeIdentityRequest: each named node that
// exists, in ascending id order. A node that does not exist (never did, or
// was deleted) is left out.
type NodeIdentityAnswer struct {
	Nodes []NodeIdentity `json:"nodes"`
}

// DecodeNodeIdentityAnswer decodes and validates an identity answer.
func DecodeNodeIdentityAnswer(data []byte) (*NodeIdentityAnswer, error) {
	var a NodeIdentityAnswer
	if err := decodeStrict(objectNodeIdentityAnswer, data, &a); err != nil {
		return nil, err
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// Validate checks the answer's rules.
func (a *NodeIdentityAnswer) Validate() error {
	if f := a.validate(); f != nil {
		return wireInvalid(objectNodeIdentityAnswer, f)
	}
	return nil
}

func (a *NodeIdentityAnswer) validate() *fieldError {
	if a.Nodes == nil {
		return &fieldError{field: "nodes", rule: RuleRequired}
	}
	if len(a.Nodes) > MaxNodeRead {
		return &fieldError{field: "nodes", rule: RuleRange}
	}
	for i, n := range a.Nodes {
		at := joinPath("nodes", strconv.Itoa(i))
		if f := nodeIDAfter(joinPath(at, "node_id"), n.NodeID, a.Nodes, i, func(x NodeIdentity) uint64 { return x.NodeID }); f != nil {
			return f
		}
		if n.Host != nil {
			if f := n.Host.validate(joinPath(at, "host")); f != nil {
				return f
			}
		}
		if f := identityCards(joinPath(at, "cards"), n.Cards); f != nil {
			return f
		}
		if f := inRange(joinPath(at, "cards_without_serial"), int64(n.CardsWithoutSerial), 0, MaxNodeCards); f != nil {
			return f
		}
	}
	return nil
}

func identityCards(path string, cards []IdentityPart) *fieldError {
	if cards == nil {
		return &fieldError{field: path, rule: RuleRequired}
	}
	if len(cards) > MaxNodeCards {
		return &fieldError{field: path, rule: RuleRange}
	}
	for j, c := range cards {
		at := joinPath(path, strconv.Itoa(j))
		if f := c.validate(at); f != nil {
			return f
		}
		if j > 0 && c.H < cards[j-1].H {
			return &fieldError{field: joinPath(at, "h"), rule: RuleOrder}
		}
	}
	return nil
}

// nodeIDAfter checks a node id of a list that must ascend strictly.
func nodeIDAfter[T any](path string, id uint64, list []T, i int, idOf func(T) uint64) *fieldError {
	if f := inRange(path, int64(min(id, maxSafeInteger+1)), 1, maxSafeInteger); f != nil {
		return f
	}
	if i > 0 {
		switch prev := idOf(list[i-1]); {
		case id == prev:
			return &fieldError{field: path, rule: RuleDuplicate}
		case id < prev:
			return &fieldError{field: path, rule: RuleOrder}
		}
	}
	return nil
}

// NodesQuery is the query of GET /host/v1/nodes: the page after Cursor
// (empty for the first page; otherwise the previous page's Next, unchanged)
// of at most Limit nodes (1 to MaxNodeRead; 0 asks for MaxNodeRead).
type NodesQuery struct {
	Cursor string
	Limit  int
}

// ParseNodesQuery reads and validates a NodesQuery. A parameter given more
// than once, or one the query does not define, is refused like a bad
// value.
func ParseNodesQuery(q url.Values) (NodesQuery, error) {
	var out NodesQuery
	for key, vals := range q {
		if key != "cursor" && key != "limit" {
			return out, newError(ErrorWireUnknownField, map[string]any{"object": objectNodesQuery, "field": key},
				"%s: unknown parameter %s", objectNodesQuery, key)
		}
		if len(vals) != 1 {
			return out, wireInvalid(objectNodesQuery, &fieldError{field: key, rule: RuleDuplicate})
		}
	}
	out.Cursor = q.Get("cursor")
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > MaxNodeRead || s != strconv.Itoa(n) {
			return out, wireInvalid(objectNodesQuery, &fieldError{field: "limit", rule: RuleRange})
		}
		out.Limit = n
	}
	if err := out.Validate(); err != nil {
		return out, err
	}
	return out, nil
}

// Validate checks the query's rules.
func (q NodesQuery) Validate() error {
	f := first(
		optionalMatches("cursor", q.Cursor, cursorPattern),
		inRange("limit", int64(q.Limit), 0, MaxNodeRead),
	)
	if f != nil {
		return wireInvalid(objectNodesQuery, f)
	}
	return nil
}

// Encode is the query string of q, without the leading "?".
func (q NodesQuery) Encode() string {
	v := url.Values{}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	if q.Limit != 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	return v.Encode()
}

// InventoryCard is one card a node may use. It holds no serial: Serial
// only reports whether the card has one and so appears, salted, in the
// node's identity.
type InventoryCard struct {
	// Index is the vendor CLI's device index, -1 when the CLI does not list
	// the card.
	Index int `json:"index"`
	// Model is the model name the probe reported, at most MaxNodeText bytes.
	Model  string `json:"model"`
	Serial bool   `json:"serial"`
}

// InventoryNode is one node of the inventory.
type InventoryNode struct {
	ID   uint64 `json:"id"`
	UUID string `json:"uuid"`
	// Name is what a person calls the node, at most MaxNodeText bytes.
	Name string `json:"name"`
	// EnrolledAt with ID gives the enrollment order.
	EnrolledAt Time `json:"enrolled_at"`
	// HostReported reports whether the node's agent reported the machine's
	// identity, which the node's identity digests.
	HostReported bool            `json:"host_reported"`
	Cards        []InventoryCard `json:"cards"`
}

// NodesPage is one page of the inventory in enrollment order (ascending
// id). Next is the cursor of the following page, absent on the last. Ids
// are never reused, so a node enrolled or deleted between pages neither
// shifts nor repeats the rest.
type NodesPage struct {
	Nodes []InventoryNode `json:"nodes"`
	Next  string          `json:"next,omitempty"`
}

// DecodeNodesPage decodes and validates an inventory page.
func DecodeNodesPage(data []byte) (*NodesPage, error) {
	var p NodesPage
	if err := decodeStrict(objectNodesPage, data, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the page's rules.
func (p *NodesPage) Validate() error {
	if f := p.validate(); f != nil {
		return wireInvalid(objectNodesPage, f)
	}
	return nil
}

func (p *NodesPage) validate() *fieldError {
	if p.Nodes == nil {
		return &fieldError{field: "nodes", rule: RuleRequired}
	}
	if len(p.Nodes) > MaxNodeRead {
		return &fieldError{field: "nodes", rule: RuleRange}
	}
	if f := optionalMatches("next", p.Next, cursorPattern); f != nil {
		return f
	}
	if p.Next != "" && len(p.Nodes) == 0 {
		return &fieldError{field: "next", rule: RuleRelation}
	}
	for i, n := range p.Nodes {
		at := joinPath("nodes", strconv.Itoa(i))
		f := first(
			nodeIDAfter(joinPath(at, "id"), n.ID, p.Nodes, i, func(x InventoryNode) uint64 { return x.ID }),
			text(joinPath(at, "uuid"), n.UUID, maxNodeUUID),
			text(joinPath(at, "name"), n.Name, MaxNodeText),
		)
		if f == nil && n.EnrolledAt.IsZero() {
			f = &fieldError{field: joinPath(at, "enrolled_at"), rule: RuleRequired}
		}
		if f == nil {
			f = inventoryCards(joinPath(at, "cards"), n.Cards)
		}
		if f != nil {
			return f
		}
	}
	return nil
}

func inventoryCards(path string, cards []InventoryCard) *fieldError {
	if cards == nil {
		return &fieldError{field: path, rule: RuleRequired}
	}
	if len(cards) > MaxNodeCards {
		return &fieldError{field: path, rule: RuleRange}
	}
	for j, c := range cards {
		at := joinPath(path, strconv.Itoa(j))
		if f := inRange(joinPath(at, "index"), int64(c.Index), -1, maxSafeInteger); f != nil {
			return f
		}
		if len(c.Model) > MaxNodeText {
			return &fieldError{field: joinPath(at, "model"), rule: RuleRange}
		}
	}
	return nil
}
