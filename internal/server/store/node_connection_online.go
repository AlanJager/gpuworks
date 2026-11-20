package store

import (
	"errors"
	"strings"
	"time"
)

// SetNodeConnected is used only inside ApplyAgentConnectionStatus after that
// transaction has checked the current token and connection ID. A follower may
// observe this authenticated connection and clear its old recovery display
// state without acquiring scheduling authority or touching the local-only
// scheduling connection.
func (s *Store) SetNodeConnected(uuid string) error {
	return s.setNodeConnected(uuid, "", "", "", "", false)
}

// SetNodeConnectedWithHandshake commits the current connection's handshake
// facts with its online projection. The enclosing owner check prevents a
// replaced connection from overwriting a newer connection's observations.
func (s *Store) SetNodeConnectedWithHandshake(uuid, version, remoteAddr, connectMode, hostname string) error {
	return s.setNodeConnected(uuid, version, remoteAddr, connectMode, hostname, true)
}

func (s *Store) setNodeConnected(uuid, version, remoteAddr, connectMode, hostname string, handshake bool) error {
	if !s.agentStatusTx || uuid == "" {
		return errors.New("node connection update requires a current agent status transaction")
	}
	updates := map[string]any{
		"state": NodeStateOnline, "last_seen_at": time.Now().UTC(),
		"recovery_phase": "", "recovery_attempts": 0,
		"last_probe_at": nil, "last_recovery_error": "",
	}
	if handshake {
		mode := "agent_dial"
		if strings.TrimSpace(connectMode) == "server_dial" {
			mode = "server_dial"
		}
		updates["agent_version"] = strings.TrimSpace(version)
		updates["last_remote_addr"] = strings.TrimSpace(remoteAddr)
		updates["connect_mode"] = mode
		updates["last_reject_reason"] = ""
		updates["last_reject_at"] = nil
	}
	result := s.db.Model(&Node{}).Where("uuid = ?", uuid).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	if name := strings.TrimSpace(hostname); handshake && name != "" {
		if err := s.db.Model(&Node{}).Where("uuid = ? AND (name = '' OR name IS NULL)", uuid).Update("name", name).Error; err != nil {
			return err
		}
	}
	return saveUnpublishedNotification(s.db, CommittedNotification{
		RequestKey: nodeNotifyKey(uuid), ResourceID: uuid, Topic: "node", Reason: "state:" + NodeStateOnline,
	})
}
