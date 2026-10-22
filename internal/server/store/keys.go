package store

import (
	"strconv"
	"strings"
)

// ModelFileKey is the model-file key used in the desired-state protocol and on
// the bus: "f<row id>". A row is identified by (model, storage, node), and the
// row id is the one short name for that triple.
func ModelFileKey(id uint) string {
	return "f" + strconv.FormatUint(uint64(id), 10)
}

// ParsedModelFileKey is a decoded model-file key. Agents started before
// storages report "<modelID>-<nodeID>"; such a key names the node's file in
// the default local storage.
type ParsedModelFileKey struct {
	ID              uint
	LegacyModelID   uint
	LegacyNodeID    uint
	LegacyFormatted bool
}

// ParseModelFileKey decodes both key formats.
func ParseModelFileKey(key string) (ParsedModelFileKey, bool) {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "f") {
		id, err := strconv.ParseUint(key[1:], 10, 64)
		if err != nil || id == 0 {
			return ParsedModelFileKey{}, false
		}
		return ParsedModelFileKey{ID: uint(id)}, true
	}
	i := strings.IndexByte(key, '-')
	if i <= 0 {
		return ParsedModelFileKey{}, false
	}
	modelID, err := strconv.ParseUint(key[:i], 10, 64)
	if err != nil || modelID == 0 {
		return ParsedModelFileKey{}, false
	}
	nodeID, err := strconv.ParseUint(key[i+1:], 10, 64)
	if err != nil {
		return ParsedModelFileKey{}, false
	}
	return ParsedModelFileKey{LegacyModelID: uint(modelID), LegacyNodeID: uint(nodeID), LegacyFormatted: true}, true
}
