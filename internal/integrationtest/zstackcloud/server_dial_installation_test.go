package zstackcloud_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provision"
)

func TestZStackCloudServerDialNormalizesUIAddresses(t *testing.T) {
	for _, tc := range []struct{ name, address, hostAddress, want string }{
		{"UI default host", "192.0.2.10", "192.0.2.30", "192.0.2.10:19091"},
		{"explicit forwarded port", "192.0.2.10:29091", "192.0.2.30", "192.0.2.10:29091"},
		{"IPv6 host", "2001:db8::10", "192.0.2.30", "[2001:db8::10]:19091"},
		{"host address fallback", "", "192.0.2.30", "192.0.2.30:19091"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := cloudInventoryFixture(t)
			requests := make(chan provision.SSHInstallRequest, 1)
			env := newCloudEnv(t, installerFunc(func(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
				requests <- req
				return provision.SSHInstallReport{}, nil
			}))
			path := addedProvider(t, env, cloud.URL)
			requestJSON(t, env, "POST", path+"/sync", `{}`, 200)
			raw := requestJSON(t, env, "POST", path+"/install", fmt.Sprintf(`{"external_id":"host-a","password":"secret","connect_mode":"server_dial","server_url":%q,"server_dial_listen":":19091","server_dial_address":%q,"host_address":%q}`, env.URL(), tc.address, tc.hostAddress), 200)
			var accepted struct {
				NodeUUID string `json:"node_uuid"`
			}
			if err := json.Unmarshal(raw, &accepted); err != nil {
				t.Fatal(err)
			}
			node, err := env.Store().NodeByUUID(accepted.NodeUUID)
			if err != nil {
				t.Fatal(err)
			}
			if node.ServerDialAddress != tc.want {
				t.Fatalf("control-plane dial target=%q want=%q", node.ServerDialAddress, tc.want)
			}
			select {
			case req := <-requests:
				// The server dials the agent: the install carries no server
				// address, although the console sent one.
				if req.ServerDialAddress != tc.want || req.ServerDialListen != ":19091" || req.ConnectMode != "server_dial" || req.ServerURL != "" {
					t.Fatalf("persisted connection choices did not reach the SSH installer: address=%q listen=%q mode=%q server_url=%q", req.ServerDialAddress, req.ServerDialListen, req.ConnectMode, req.ServerURL)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("installation was not dispatched")
			}
		})
	}
}
