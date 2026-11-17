package hub

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// recordHandshakeFacts persists what an agent's hello says about its node.
// It runs before the node reads as online, exactly as in the single-lane path
// where register ran after the whole handshake, so its agent version, remote
// address and models directory are already on record by then.
func (s *Session) recordHandshakeFacts(n *store.Node, hello proto.Hello) {
	if err := s.hub.store.SetNodeAgentInfo(n.UUID, hello.Version, s.remote, hello.ConnectMode); err != nil {
		s.hub.log.Warn("persist agent handshake info failed", "node", n.UUID, "err", err)
	}
	if dir := strings.TrimSpace(hello.OwnModelsDir); dir != "" {
		// The node's first model download must already go to a directory its
		// agent can write.
		if err := s.hub.store.EnsureNodeOwnModelsDir(n, dir); err != nil {
			s.hub.log.Warn("record node models directory failed", "node", n.UUID, "err", err)
		}
	}
	if id := proto.NormalizeHostSystemUUID(hello.HostSystemUUID); id != "" && id != n.HostSystemUUID {
		// Written only when it differs from the row authentication already
		// read, so a routine reconnect adds no write; an empty value (an old
		// or non-root agent) keeps what an earlier agent reported.
		if _, err := s.hub.store.SetNodeHostSystemUUID(n.UUID, id); err != nil {
			s.hub.log.Warn("record node host system uuid failed", "node", n.UUID, "err", err)
		}
	}
	if n.Name == "" && strings.TrimSpace(hello.Hostname) != "" {
		// The first observed hostname is a useful default display name. Once a
		// user sets Name, it is never overwritten by reconnects.
		_ = s.hub.store.SetNodeNameIfEmpty(n.UUID, hello.Hostname)
	}
}
