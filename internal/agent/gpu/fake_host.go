package gpu

import (
	"fmt"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// FakeHost is a test-only host with cards of several vendors, selected by
// GPUWORKS_FAKE_GPU_VENDORS in the standalone agent, so a process test can
// choose between them and watch the agent restart into the other vendor's
// cards. Each vendor's cards come from a FixedProber; the census lists them
// all, as a real host's PCI bus would.
type FakeHost struct {
	vendors []fakeVendor
}

type fakeVendor struct {
	vendor, model string
	cards         int
}

// FakeHostFromEnv parses GPUWORKS_FAKE_GPU_VENDORS=<vendor>:<model>x<count>[;...],
// for example "ascend:310P3x1;nvidia:RTX 4090x1". An empty value returns nil.
func FakeHostFromEnv(value string) (*FakeHost, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	host := &FakeHost{}
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ";") {
		vendor, cards, ok := strings.Cut(strings.TrimSpace(part), ":")
		vendor = strings.ToLower(strings.TrimSpace(vendor))
		x := strings.LastIndex(strings.ToLower(cards), "x")
		if !ok || !ChoosableVendor(vendor) || seen[vendor] || x <= 0 {
			return nil, fmt.Errorf("GPUWORKS_FAKE_GPU_VENDORS must be vendor:MODELxCOUNT;..., got %q", value)
		}
		count, err := strconv.Atoi(cards[x+1:])
		if err != nil || count <= 0 || count > 64 {
			return nil, fmt.Errorf("invalid fake card count in %q", part)
		}
		seen[vendor] = true
		host.vendors = append(host.vendors, fakeVendor{vendor: vendor, model: strings.TrimSpace(cards[:x]), cards: count})
	}
	return host, nil
}

// ProberFor is the prober for the chosen vendor's cards; automatic, or a
// vendor the host lacks, gets the vendor New would pick first.
func (h *FakeHost) ProberFor(preference string) Prober {
	chosen := h.vendors[0]
	for _, p := range defaultProbers() {
		if v, ok := h.vendor(p.Vendor()); ok {
			chosen = v
			break
		}
	}
	if v, ok := h.vendor(preference); ok {
		chosen = v
	}
	// NVIDIA spec predicates match on compute capability, so the fake
	// NVIDIA cards carry one (an Ada card's) as GPUWORKS_FAKE_GPU's do.
	cc := ""
	if chosen.vendor == accelerator.VendorNVIDIA {
		cc = "8.9"
	}
	gpus := make([]proto.GPUInfo, chosen.cards)
	for i := range gpus {
		gpus[i] = proto.GPUInfo{
			Index: i, UUID: fmt.Sprintf("FAKE-%s-%d", strings.ToUpper(chosen.vendor), i), Name: chosen.model,
			MemoryTotalBytes: 24 << 30, Vendor: chosen.vendor, Healthy: true, ComputeCapability: cc,
			PCIeBDF: fmt.Sprintf("0000:%02x:00.0", 0x10*(h.position(chosen.vendor)+1)+i),
		}
	}
	return &FixedProber{GPUs: gpus, VendorName: chosen.vendor}
}

// Vendors lists every vendor's cards, in vendor order as VendorCensus does.
func (h *FakeHost) Vendors() []proto.AcceleratorPresence {
	out := make([]proto.AcceleratorPresence, 0, len(h.vendors))
	for _, p := range []string{"ascend", "hygon", "nvidia", "ppu"} {
		if v, ok := h.vendor(p); ok {
			out = append(out, proto.AcceleratorPresence{Vendor: v.vendor, Cards: v.cards, Model: v.model})
		}
	}
	return out
}

func (h *FakeHost) vendor(name string) (fakeVendor, bool) {
	for _, v := range h.vendors {
		if v.vendor == name {
			return v, true
		}
	}
	return fakeVendor{}, false
}

func (h *FakeHost) position(name string) int {
	for i, v := range h.vendors {
		if v.vendor == name {
			return i
		}
	}
	return 0
}
