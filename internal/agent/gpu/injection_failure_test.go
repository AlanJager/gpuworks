package gpu

import (
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestKunlunTransportFailureCarriesStableResource(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		xdr      bool
		resource string
	}{
		{nil, true, "/dev/infiniband"},
		{[]string{"--device", "/dev/infiniband", "--ulimit", "memlock=4096", "--cap-add", "IPC_LOCK"}, true, "memlock"},
		{[]string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1"}, true, "IPC_LOCK"},
		{[]string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"}, false, "/dev/xdrdrv"},
	} {
		p, _ := kunlunFixture(t)
		if tc.xdr {
			withKunlunXDR(p, os.ModeDevice|os.ModeCharDevice)
		}
		got := p.RuntimeInjectionForLaunch(t.Context(), []int{0}, "docker", 2, tc.args)
		if got.Failure == nil || got.Failure.Code != proto.ErrorAcceleratorResourceUnavailable || got.Failure.Resource != tc.resource || got.UnsupportedReason == "" || len(got.DockerArgs) != 0 || len(got.Env) != 0 {
			t.Fatalf("resource=%s got=%+v", tc.resource, got)
		}
	}
}
