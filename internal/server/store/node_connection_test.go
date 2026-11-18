package store_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNodeConnectionPresentationSeparatesSessionsFromStoredFacts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		node      *store.Node
		connected bool
		want      string
	}{
		{"nil", nil, false, ""},
		{"old online", &store.Node{State: store.NodeStateOnline}, false, store.NodeStateConnecting},
		{"explicit offline", &store.Node{State: store.NodeStateOffline}, false, store.NodeStateOffline},
		{"pending", &store.Node{State: store.NodeStateConnecting}, false, store.NodeStateConnecting},
		{"authenticated before write", &store.Node{State: store.NodeStateOffline}, true, store.NodeStateOnline},
		{"existing recovery", &store.Node{State: store.NodeStateOffline, RecoveryPhase: "probing"}, false, store.NodeStateConnecting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.node.ConnectionStateForSession(tc.connected); got != tc.want {
				t.Fatalf("connection state=%q want=%q", got, tc.want)
			}
		})
	}
}
