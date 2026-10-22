package gpu

import (
	"context"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The fake mixed host answers as a real one does: automatic picks the
// vendor New would try first, a chosen vendor gets its own cards, and the
// census lists every vendor whichever is managed.
func TestTheFakeMixedHostChoosesLikeARealOne(t *testing.T) {
	host, err := FakeHostFromEnv(" nvidia:RTX 4090x1 ; ascend:310P3x2 ")
	if err != nil {
		t.Fatal(err)
	}
	for preference, want := range map[string]string{"auto": "ascend", "": "ascend", "nvidia": "nvidia", "ascend": "ascend", "hygon": "ascend"} {
		p := host.ProberFor(preference)
		if p.Vendor() != want {
			t.Fatalf("ProberFor(%q) = %s, want %s", preference, p.Vendor(), want)
		}
		gpus, err := p.Probe(context.Background())
		if err != nil || len(gpus) == 0 || gpus[0].Vendor != want {
			t.Fatalf("ProberFor(%q) cards = %#v, %v", preference, gpus, err)
		}
	}
	nvidia, _ := host.ProberFor("nvidia").Probe(context.Background())
	ascend, _ := host.ProberFor("ascend").Probe(context.Background())
	if len(nvidia) != 1 || nvidia[0].Name != "RTX 4090" || len(ascend) != 2 || ascend[1].Name != "310P3" {
		t.Fatalf("cards nvidia=%#v ascend=%#v", nvidia, ascend)
	}
	if nvidia[0].PCIeBDF == ascend[0].PCIeBDF || ascend[0].PCIeBDF == ascend[1].PCIeBDF {
		t.Fatalf("cards share an address: %#v %#v", nvidia, ascend)
	}
	want := []proto.AcceleratorPresence{{Vendor: "ascend", Cards: 2, Model: "310P3"}, {Vendor: "nvidia", Cards: 1, Model: "RTX 4090"}}
	if got := host.Vendors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Vendors() = %#v, want %#v", got, want)
	}
}

func TestTheFakeMixedHostRejectsWhatItCannotModel(t *testing.T) {
	if host, err := FakeHostFromEnv(""); host != nil || err != nil {
		t.Fatalf("empty = %v, %v", host, err)
	}
	for _, bad := range []string{"nvidia", "apple:M1x1", "nvidia:A10x0", "nvidia:A10x1;nvidia:L4x1", "nvidia:x1", "nvidia:A10xx"} {
		if _, err := FakeHostFromEnv(bad); err == nil {
			t.Fatalf("FakeHostFromEnv(%q) accepted", bad)
		}
	}
}
