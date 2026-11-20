package proto

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// RPCNodeCapabilities is a control-plane observation, handled by the session
// owner Hub. It must never be sent to an agent as an RPC or grant action.
const RPCNodeCapabilities = "hub.node.capabilities"

// NodeCapabilitiesResult binds the advertised protocols to the exact current
// connection. It proves neither action authority nor resource readiness.
type NodeCapabilitiesResult struct {
	NodeUUID          string   `json:"node_uuid"`
	OwnerMemberID     uint64   `json:"owner_member_id"`
	SessionGeneration uint64   `json:"session_generation"`
	SessionUUID       string   `json:"session_uuid"`
	ConnectionID      string   `json:"connection_id"`
	Caps              []string `json:"caps"`
}

func ValidateNodeCapabilitiesParams(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if !utf8.Valid(raw) {
		return errors.New("invalid node capability query encoding")
	}
	return decodeModelDownloadControl(raw, map[string]any{})
}

func ValidateNodeCapabilitiesResult(p NodeCapabilitiesResult) error {
	for _, text := range []string{p.NodeUUID, p.SessionUUID, p.ConnectionID} {
		if !validActionGrantText(text) || strings.TrimSpace(text) == "" {
			return errors.New("node capabilities have no current connection identity")
		}
	}
	if p.OwnerMemberID == 0 || p.SessionGeneration == 0 || p.Caps == nil {
		return errors.New("node capabilities require an identified connection and explicit capability array")
	}
	for _, cap := range p.Caps {
		if !validActionGrantText(cap) || strings.TrimSpace(cap) == "" {
			return errors.New("invalid advertised node capability")
		}
	}
	return nil
}

func DecodeNodeCapabilitiesResult(raw json.RawMessage) (NodeCapabilitiesResult, error) {
	var p NodeCapabilitiesResult
	if !utf8.Valid(raw) {
		return p, errors.New("invalid node capability result encoding")
	}
	err := decodeModelDownloadControl(raw, map[string]any{"node_uuid": &p.NodeUUID, "owner_member_id": &p.OwnerMemberID, "session_generation": &p.SessionGeneration, "session_uuid": &p.SessionUUID, "connection_id": &p.ConnectionID, "caps": &p.Caps})
	if err == nil {
		err = ValidateNodeCapabilitiesResult(p)
	}
	if err != nil {
		return NodeCapabilitiesResult{}, err
	}
	return p, nil
}
