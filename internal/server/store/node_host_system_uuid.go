package store

import "gpuworks.ai/gpuworks/internal/proto"

// SetNodeHostSystemUUID records the firmware system UUID the node's agent
// reported (Node.HostSystemUUID) and reports whether it changed. The value
// is normalized again (proto.NormalizeHostSystemUUID): the server never
// trusts what an agent sends. An empty or unusable value records nothing,
// so an agent that cannot read it, or one that predates it, never erases
// what an earlier agent on the machine reported. The caller compares
// against the row it already holds and calls only on a difference, so a
// routine reconnect costs no write.
func (s *Store) SetNodeHostSystemUUID(uuid, value string) (bool, error) {
	value = proto.NormalizeHostSystemUUID(value)
	if value == "" {
		return false, nil
	}
	res := s.db.Model(&Node{}).Where("uuid = ? AND host_system_uuid <> ?", uuid, value).Update("host_system_uuid", value)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	s.publishNode(uuid, "host_system_uuid")
	return true, nil
}
