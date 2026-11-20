package proto

import (
	"encoding/json"
	"errors"
)

// ImageStreamActionParams is the canonical payload signed for one image
// stream. Only dst and ref affect image save/load; every other header field
// must be absent so no executable option escapes the signed scope.
func ImageStreamActionParams(h StreamHeader) (json.RawMessage, error) {
	if (h.Kind != StreamImageRead && h.Kind != StreamImageWrite) || h.Dst == "" || h.Ref == "" ||
		h.Target != "" || h.Rows != 0 || h.Cols != 0 || h.GraceSec != 0 {
		return nil, errors.New("invalid image stream action header")
	}
	return json.Marshal(struct {
		Dst string `json:"dst"`
		Ref string `json:"ref"`
	}{Dst: h.Dst, Ref: h.Ref})
}

func ImageStreamActionPayloadSHA256(h StreamHeader) (string, error) {
	params, err := ImageStreamActionParams(h)
	if err != nil {
		return "", err
	}
	return RPCActionPayloadSHA256(h.Kind, params)
}
