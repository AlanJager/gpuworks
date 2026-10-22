package api

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeInstallCommandFor is the agent bootstrap command for a node that
// already exists. It is handed out as the way to bring back a node whose
// agent the platform cannot restore on its own (no SSH credential, or
// recovery gave up), which is the same act as the first install — so it is
// the same command, from the same place (provision.InstallCommand)
// rather than a second spelling of it here.
//
// It carries the node's token, so it is only ever built where the token
// itself is returned. A node the server dials gets the offline package's
// command, which names no server.
func nodeInstallCommandFor(n *store.Node) string {
	serverURL := strings.TrimRight(strings.TrimSpace(n.ServerURL), "/")
	if n.Token == "" || (serverURL == "" && n.ConnectMode != "server_dial") {
		// Nothing to hand over: a node registered before the server knew
		// its own URL, or one whose token is not being returned.
		return ""
	}
	return provision.InstallCommand(serverURL, n.Token, n.ConnectMode, n.ServerDialListen)
}
