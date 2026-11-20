package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const (
	RPCObservationCancel = "observation.cancel"
	RPCObservationRenew  = "observation.renew"
)

// ObservationActionControlParams identifies the original action. Synchronous
// observations and listeners use separate decoders for their allowed families.
// Params carries the exact original bytes; JSON's base64 byte encoding prevents
// a management envelope from silently changing an already signed JSON body.
type ObservationActionControlParams struct {
	GrantID      string `json:"grant_id"`
	ActionFamily string `json:"action_family"`
	Params       []byte `json:"params"`
}

type ObservationActionCancelResult struct {
	GrantID string `json:"grant_id"`
	Stopped bool   `json:"stopped"`
}

type ObservationActionRenewResult struct {
	GrantID        string `json:"grant_id"`
	NotAfterUnixMS int64  `json:"not_after_unix_ms"`
}

func DecodeObservationActionControl(raw json.RawMessage) (ObservationActionControlParams, error) {
	return decodeActionControl(raw, IsObservationAction, "observation")
}

func decodeActionControl(raw json.RawMessage, allowed func(string) bool, kind string) (ObservationActionControlParams, error) {
	var p ObservationActionControlParams
	if len(raw) == 0 || len(raw) > 1<<20 {
		return p, errors.New("invalid " + kind + " control size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return p, errors.New(kind + " control must be an object")
	}
	seen := make(map[string]bool, 3)
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return ObservationActionControlParams{}, err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return ObservationActionControlParams{}, errors.New("duplicate " + kind + " control field")
		}
		seen[name] = true
		switch name {
		case "grant_id":
			err = d.Decode(&p.GrantID)
		case "action_family":
			err = d.Decode(&p.ActionFamily)
		case "params":
			err = d.Decode(&p.Params)
		default:
			return ObservationActionControlParams{}, errors.New("unknown " + kind + " control field")
		}
		if err != nil {
			return ObservationActionControlParams{}, err
		}
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return ObservationActionControlParams{}, errors.New("invalid " + kind + " control object")
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ObservationActionControlParams{}, errors.New("trailing " + kind + " control JSON")
	}
	if p.GrantID == "" || len(p.GrantID) > 128 || !allowed(p.ActionFamily) || len(p.Params) == 0 || !json.Valid(p.Params) {
		return ObservationActionControlParams{}, errors.New("invalid " + kind + " action identity")
	}
	return p, nil
}
