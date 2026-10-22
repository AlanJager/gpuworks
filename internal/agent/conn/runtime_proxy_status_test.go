package conn

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

type runtimeProxyHandlers struct {
	inboundHandlers
	status *proto.RuntimeProxyStatus
}

func (h *runtimeProxyHandlers) RuntimeProxyStatus(context.Context) *proto.RuntimeProxyStatus {
	return h.status
}

func nodeStatusWire(t *testing.T, h Handlers) map[string]json.RawMessage {
	t.Helper()
	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, h, &gpu.FixedProber{VendorName: "test"}, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	line, _, _ := strings.Cut(string(ctrl.BytesCopy()), "\n")
	var env struct {
		D map[string]json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("%v in %s", err, line)
	}
	return env.D
}

func TestNodeStatusCarriesTheRuntimeProxyOnlyWhenTheRuntimeHasOne(t *testing.T) {
	withProxy := nodeStatusWire(t, &runtimeProxyHandlers{status: &proto.RuntimeProxyStatus{
		HTTPProxy: "http://xxxxx:xxxxx@proxy.corp:3128", NoProxy: "localhost", WrittenHTTPProxy: "http://p:1",
		LiveRestore: true, Systemd: true, Privilege: proto.RuntimePrivilegeSudo, Error: "docker info failed",
	}})
	var got proto.RuntimeProxyStatus
	if err := json.Unmarshal(withProxy["runtime_proxy"], &got); err != nil {
		t.Fatalf("runtime_proxy missing or malformed in %v: %v", withProxy, err)
	}
	if got.HTTPProxy != "http://xxxxx:xxxxx@proxy.corp:3128" || got.WrittenHTTPProxy != "http://p:1" || !got.LiveRestore || !got.Systemd || got.Privilege != "sudo" || got.Error != "docker info failed" {
		t.Fatalf("runtime_proxy = %+v", got)
	}

	// A runtime that pulls in the agent's own process reports none, and an
	// agent that predates the field never had a handler for it.
	for name, h := range map[string]Handlers{
		"runtime without a daemon": &runtimeProxyHandlers{},
		"handler without the hook": &inboundHandlers{},
	} {
		if raw, present := nodeStatusWire(t, h)["runtime_proxy"]; present {
			t.Errorf("%s reported runtime_proxy %s", name, raw)
		}
	}
}
