package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// InstanceActionPayloadSHA256 hashes the exact per-instance body placed on
// the wire, including any runtime asset bytes added for this agent. Callers
// must compute it after the Hub finishes shaping that body. The agent hashes
// the decoded item again before accepting or executing the target.
func InstanceActionPayloadSHA256(spec InstanceDesiredState) (string, error) {
	return actionPayloadSHA256(spec)
}

// InstanceActionTargetSHA256 binds the persisted business target while allowing
// the Hub to include asset bytes only for agents that do not already cache them.
// Asset identity, digest, size, package and every other launch field remain in
// the hash; only transient content is removed from a copy of the body.
func InstanceActionTargetSHA256(spec InstanceDesiredState) (string, error) {
	spec.ResolvedLaunch.RuntimeAssets = append([]RuntimeAsset(nil), spec.ResolvedLaunch.RuntimeAssets...)
	for i := range spec.ResolvedLaunch.RuntimeAssets {
		spec.ResolvedLaunch.RuntimeAssets[i].Content = nil
	}
	return actionPayloadSHA256(spec)
}

// RemovalActionPayloadSHA256 binds one versioned removal intent. A different
// removal version requires a new authorization even for the same instance.
func RemovalActionPayloadSHA256(intent InstanceRemovalIntent) (string, error) {
	return actionPayloadSHA256(intent)
}

func actionPayloadSHA256(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
