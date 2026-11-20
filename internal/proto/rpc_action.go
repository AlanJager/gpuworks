package proto

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const rpcActionPayloadDomain = "gpuworks.rpc-action.v1\x00"

// RPCActionPayloadSHA256 binds both the RPC method and the exact JSON bytes
// sent as its params. Re-encoding the decoded params would accept a different
// request body under the same ticket, so callers must pass the wire bytes.
func RPCActionPayloadSHA256(method string, params json.RawMessage) (string, error) {
	if method == "" || len(params) == 0 || !json.Valid(params) {
		return "", errors.New("invalid RPC action method or params")
	}
	b := []byte(rpcActionPayloadDomain)
	b = binary.AppendUvarint(b, uint64(len(method)))
	b = append(b, method...)
	b = binary.AppendUvarint(b, uint64(len(params)))
	b = append(b, params...)
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}
