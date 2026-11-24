package gpu

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type vendorOnlyProber struct {
	Prober
	vendor string
	inj    RuntimeInjection
}

func (p vendorOnlyProber) Vendor() string                          { return p.vendor }
func (p vendorOnlyProber) RuntimeInjection([]int) RuntimeInjection { return p.inj }

// The vendor is named where the injection is resolved, in its canonical
// spelling, so no backend can grant devices without saying whose they are; a
// refusal and an empty selection grant nothing and name nobody.
func TestResolveInjectionNamesTheVendor(t *testing.T) {
	granted := RuntimeInjection{Grant: runtime.DeviceGrant{Devices: []runtime.Device{{Path: "/dev/card"}}}}
	for _, tc := range []struct {
		name    string
		prober  Prober
		indexes []int
		want    string
	}{
		{"canonical name", vendorOnlyProber{vendor: accelerator.VendorHygon, inj: granted}, []int{0}, accelerator.VendorHygon},
		{"an alias is normalized", vendorOnlyProber{vendor: "Huawei", inj: granted}, []int{0}, accelerator.VendorAscend},
		{"no cards selected", vendorOnlyProber{vendor: accelerator.VendorHygon, inj: granted}, nil, ""},
		{"refused", vendorOnlyProber{vendor: accelerator.VendorHygon, inj: RuntimeInjection{UnsupportedReason: "card is gone"}}, []int{0}, ""},
		{
			"refused with a typed failure",
			vendorOnlyProber{vendor: accelerator.VendorHygon, inj: rejectedInjection(proto.RuntimeFailure{Code: proto.ErrorAcceleratorDeviceUnavailable}, "card is gone")},
			[]int{0}, "",
		},
		{"no prober", nil, []int{0}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inj := ResolveInjection(context.Background(), tc.prober, InjectionRequest{Indexes: tc.indexes, DriverName: "docker"})
			if inj.Grant.Vendor != tc.want {
				t.Fatalf("Vendor = %q, want %q", inj.Grant.Vendor, tc.want)
			}
		})
	}
}
