package hostsim

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestConnectionDiagnosticsReadsOnlySimulatedConnectionRecords(t *testing.T) {
	h := &Host{Dir: t.TempDir()}
	if err := h.RPC(proto.RPCNodeConnectionDiagnostics, nil); err != nil {
		t.Fatalf("own connection snapshot refused: %v", err)
	}
	if err := h.RPC(proto.RPCNodeDoctor, nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("hardware doctor must remain refused: %v", err)
	}
}
