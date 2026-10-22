package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu/pcicards"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The cards of zstone-164 as sysfs reports them: two passed through to
// virtual machines, one with no driver, five held by the nvidia driver — of
// which it brought up only two.
func zstone164Cards() []pcicards.Card {
	card := func(bdf, driver string) pcicards.Card {
		return pcicards.Card{BDF: bdf, Vendor: accelerator.VendorNVIDIA, VendorID: 0x10de, DeviceID: 0x2204, Driver: driver, NUMANode: 0}
	}
	return []pcicards.Card{
		card("0000:1a:00.0", pcicards.DriverNone),
		card("0000:1b:00.0", pcicards.DriverVFIO),
		card("0000:1c:00.0", "nvidia"),
		card("0000:3f:00.0", "nvidia"),
		card("0000:40:00.0", "nvidia"),
	}
}

func byBDF(gpus []proto.GPUInfo) map[string]proto.GPUInfo {
	out := make(map[string]proto.GPUInfo, len(gpus))
	for _, g := range gpus {
		out[accelerator.NormalizeBDF(g.PCIeBDF)] = g
	}
	return out
}

// Every card in the machine is reported, each saying what it is: the ones
// the driver answered for keep its facts, and the ones it did not say why.
func TestRosterReportsEveryCardAndWhyTheCLIOmittedIt(t *testing.T) {
	listed := []proto.GPUInfo{
		{Index: 0, UUID: "GPU-3f", Name: "NVIDIA GeForce RTX 3090", PCIeBDF: "00000000:3F:00.0", MemoryTotalBytes: 24 << 30, Healthy: true, NUMANode: 0},
		{Index: 1, UUID: "GPU-40", Name: "NVIDIA GeForce RTX 3090", PCIeBDF: "00000000:40:00.0", MemoryTotalBytes: 24 << 30, Healthy: true, NUMANode: 0},
	}
	names := map[string]string{"0000:1a:00.0": "GA102 [GeForce RTX 3090]", "0000:1b:00.0": "GA102 [GeForce RTX 3090]", "0000:1c:00.0": "GA102 [GeForce RTX 3090]"}

	roster := mergeRoster(listed, zstone164Cards(), names, true)
	if len(roster) != 5 {
		t.Fatalf("roster has %d cards, want all 5 the host has: %+v", len(roster), roster)
	}
	got := byBDF(roster)

	// The driver answered for these two: its facts are kept untouched.
	for _, bdf := range []string{"0000:3f:00.0", "0000:40:00.0"} {
		card := got[bdf]
		if card.UUID == "" || card.MemoryTotalBytes == 0 || !card.Healthy || card.Ownership != accelerator.CardOwnedByNode || card.Index < 0 {
			t.Fatalf("%s = %+v, want the CLI's own facts", bdf, card)
		}
	}

	// Bound to nvidia and not listed: this node's card, and unusable.
	broken := got["0000:1c:00.0"]
	if broken.Ownership != accelerator.CardOwnedByNode || broken.HealthCode != accelerator.HealthDriverInitFailed {
		t.Fatalf("the card its driver could not bring up = %+v", broken)
	}
	if broken.HealthStatus != accelerator.HealthError || broken.Healthy {
		t.Fatalf("a card that cannot be used must not read as healthy: %+v", broken)
	}
	if broken.Index != -1 {
		t.Fatalf("index = %d, want -1: an index is what the CLI hands out", broken.Index)
	}
	// The CLI named this card's siblings on this very host, so it is
	// called what they are called rather than what pci.ids calls the
	// model ("GA102 [GeForce RTX 3090]"). One list, one name per model.
	if broken.Name != "NVIDIA GeForce RTX 3090" {
		t.Fatalf("name = %q, want what the CLI calls this model on this host", broken.Name)
	}

	// Passed through to a VM: present, not ours, and not a fault.
	vm := got["0000:1b:00.0"]
	if vm.Ownership != accelerator.CardPassedThrough || vm.HealthCode != "" || vm.HealthStatus != "" {
		t.Fatalf("a passed-through card = %+v, want no health verdict at all", vm)
	}
	if vm.Healthy {
		t.Fatal("a card this node cannot touch must not read as healthy")
	}

	// No driver bound: the hardware is there and nothing can talk to it.
	bare := got["0000:1a:00.0"]
	if bare.Ownership != accelerator.CardWithoutDriver || bare.HealthCode != "" || bare.Healthy {
		t.Fatalf("a card with no driver = %+v", bare)
	}
}

// A CLI that did not answer at all says nothing about any card. The cards
// are still reported — that is the whole point of reading PCI — but the
// reason is "we could not ask", not "the driver could not bring it up":
// a 60-second nvidia-smi must not make a working card look broken.
func TestRosterSeparatesACLIThatDidNotAnswerFromACardItLeftOut(t *testing.T) {
	roster := mergeRoster(nil, zstone164Cards(), nil, false)
	got := byBDF(roster)
	for _, bdf := range []string{"0000:1c:00.0", "0000:3f:00.0", "0000:40:00.0"} {
		card := got[bdf]
		if card.HealthCode != accelerator.HealthCLIUnanswered {
			t.Fatalf("%s = %q, want the card to say the CLI did not answer", bdf, card.HealthCode)
		}
		if card.Ownership != accelerator.CardOwnedByNode || card.Healthy {
			t.Fatalf("%s = %+v, want this node's card, not usable until we can ask", bdf, card)
		}
	}
	// Ownership is read from sysfs, not from the CLI, so it still holds.
	if got["0000:1b:00.0"].Ownership != accelerator.CardPassedThrough || got["0000:1b:00.0"].HealthCode != "" {
		t.Fatalf("a passed-through card = %+v, want no health verdict even when the CLI is silent", got["0000:1b:00.0"])
	}
}

// A card with no name in pci.ids is named by its ids, as lspci prints them,
// rather than left blank.
func TestRosterNamesAnUnknownCardByItsIDs(t *testing.T) {
	cards := []pcicards.Card{{BDF: "0000:1c:00.0", Vendor: accelerator.VendorNVIDIA, VendorID: 0x10de, DeviceID: 0x9999, Driver: "nvidia", NUMANode: -1}}
	roster := mergeRoster(nil, cards, nil, true)
	if len(roster) != 1 || roster[0].Name != "10de:9999" {
		t.Fatalf("roster = %+v", roster)
	}
}

// The CLI's PCI address spelling has eight domain digits and sysfs four;
// the same card must not appear twice.
func TestRosterMatchesTheCLIAndSysfsSpellingsOfOneAddress(t *testing.T) {
	listed := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", PCIeBDF: "00000000:1C:00.0", Healthy: true}}
	cards := []pcicards.Card{{BDF: "0000:1c:00.0", Vendor: accelerator.VendorNVIDIA, Driver: "nvidia"}}
	roster := mergeRoster(listed, cards, nil, true)
	if len(roster) != 1 || roster[0].UUID != "GPU-a" {
		t.Fatalf("roster = %+v, want the one card with the CLI's facts", roster)
	}
}

// A container maps its cards' device nodes without mapping PCI sysfs. The
// CLI's list is then all there is, and it is used as it always was.
func TestRosterKeepsCLICardsPCIDoesNotShow(t *testing.T) {
	listed := []proto.GPUInfo{
		{Index: 1, UUID: "GPU-b", PCIeBDF: "0000:40:00.0", Healthy: true},
		{Index: 0, UUID: "GPU-a", PCIeBDF: "0000:3f:00.0", Healthy: true},
	}
	roster := mergeRoster(listed, nil, nil, true)
	if len(roster) != 2 || roster[0].Index != 0 || roster[1].Index != 1 {
		t.Fatalf("roster = %+v, want both cards in index order", roster)
	}
	for _, card := range roster {
		if card.Ownership != accelerator.CardOwnedByNode {
			t.Fatalf("a card only the CLI knows = %+v, want it owned by this node", card)
		}
	}
}

// PCI fills in only what the CLI left blank.
func TestRosterLetsTheDriverWinOverPCI(t *testing.T) {
	listed := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", Name: "NVIDIA A100-SXM4-80GB", PCIeBDF: "0000:3f:00.0", NUMANode: 0, Healthy: true}}
	cards := []pcicards.Card{{BDF: "0000:3f:00.0", Vendor: accelerator.VendorNVIDIA, Driver: "nvidia", NUMANode: 1}}
	roster := mergeRoster(listed, cards, map[string]string{"0000:3f:00.0": "GA100 [A100 SXM4 80GB]"}, true)
	if roster[0].Name != "NVIDIA A100-SXM4-80GB" {
		t.Fatalf("name = %q, want the driver's own", roster[0].Name)
	}
	if roster[0].NUMANode != 1 {
		t.Fatalf("numa = %d; sysfs fills in what the CLI reported as 0", roster[0].NUMANode)
	}
}

// Without a readable PCI sysfs the prober behaves exactly as before.
func TestProbeFallsBackToTheCLIWithoutPCISysfs(t *testing.T) {
	p := &SMIProber{hostRoot: t.TempDir(), runner: func(_ context.Context, args ...string) (string, error) {
		if args[0] == "-L" {
			return "", nil
		}
		return "0, GPU-a, NVIDIA A100-SXM4-80GB, 81920, 8.0, 00000000:3F:00.0, 550.54.15\n", nil
	}}
	gpus, err := p.Probe(context.Background())
	if err != nil || len(gpus) != 1 || gpus[0].UUID != "GPU-a" || gpus[0].Index != 0 {
		t.Fatalf("probe = %+v, %v", gpus, err)
	}
}

// hostWithCards builds a fixture /sys the prober's own scanner reads, so
// Probe's whole path is exercised rather than only the merge.
func hostWithCards(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, card := range []struct{ bdf, device, driver string }{
		{"0000:1b:00.0", "0x2204", "vfio-pci"},
		{"0000:1c:00.0", "0x2204", "nvidia"},
		{"0000:3f:00.0", "0x2204", "nvidia"},
	} {
		device := filepath.Join(root, "sys/bus/pci/devices", card.bdf)
		if err := os.MkdirAll(device, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{"vendor": "0x10de", "device": card.device, "class": "0x030000", "numa_node": "0"} {
			if err := os.WriteFile(filepath.Join(device, name), []byte(value+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("../../../../bus/pci/drivers/"+card.driver, filepath.Join(device, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	// Another vendor's accelerator (an Ascend 910B4), which this prober
	// must leave to its own vendor's prober, and the BMC display
	// controller that shares Huawei's vendor id, which is nobody's card.
	for _, other := range []struct{ bdf, device, class string }{
		{"0000:c1:00.0", "0xd802", "0x120000"},
		{"0000:03:00.0", "0x1711", "0x030000"},
	} {
		device := filepath.Join(root, "sys/bus/pci/devices", other.bdf)
		if err := os.MkdirAll(device, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{"vendor": "0x19e5", "device": other.device, "class": other.class} {
			if err := os.WriteFile(filepath.Join(device, name), []byte(value+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// Probe end to end on a host whose CLI lists one of three cards: the other
// two are reported with what sysfs knows and why the CLI left them out.
func TestProbeReportsTheHostsCardsThroughTheScanner(t *testing.T) {
	p := &SMIProber{hostRoot: hostWithCards(t), runner: func(_ context.Context, args ...string) (string, error) {
		if args[0] == "-L" {
			return "", nil
		}
		return "0, GPU-3f, NVIDIA GeForce RTX 3090, 24576, 8.6, 00000000:3F:00.0, 580.126.09\n", nil
	}}
	gpus, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe = %v", err)
	}
	if len(gpus) != 3 {
		t.Fatalf("cards = %d, want the three NVIDIA cards sysfs shows: %+v", len(gpus), gpus)
	}
	got := byBDF(gpus)
	listed := got["0000:3f:00.0"]
	if listed.UUID != "GPU-3f" || listed.Index != 0 || listed.MemoryTotalBytes == 0 {
		t.Fatalf("the listed card lost the CLI's facts: %+v", listed)
	}
	if broken := got["0000:1c:00.0"]; broken.HealthCode != accelerator.HealthDriverInitFailed || broken.Index != -1 {
		t.Fatalf("the card its driver could not bring up = %+v", broken)
	}
	if vm := got["0000:1b:00.0"]; vm.Ownership != accelerator.CardPassedThrough {
		t.Fatalf("the passed-through card = %+v", vm)
	}
	if _, found := got["0000:c1:00.0"]; found {
		t.Fatal("another vendor's card was reported by the NVIDIA prober")
	}
}

// With no CLI at all the cards are still reported: a host with four cards
// and no driver is a useful thing to be told, and reporting none sends an
// operator looking for hardware that is sitting right there.
func TestProbeWithoutTheCLIStillReportsTheCards(t *testing.T) {
	p := &SMIProber{hostRoot: hostWithCards(t), runner: func(context.Context, ...string) (string, error) {
		return "", errors.New("nvidia-smi: executable file not found in $PATH")
	}}
	gpus, err := p.Probe(context.Background())
	if err == nil {
		t.Fatal("the probe reported success although the CLI could not be asked")
	}
	if len(gpus) != 3 {
		t.Fatalf("cards = %+v, want the three sysfs shows", gpus)
	}
	for _, card := range gpus {
		if card.Index != -1 {
			t.Fatalf("a card got a device index although the CLI numbered none: %+v", card)
		}
	}
}

// Some of a host's cards are handed to virtual machines and the rest are
// not. The vendor CLI names the rest, and that is this model's name from
// the vendor's own tool on this very machine — better than any database,
// and the one thing that keeps a list from calling the same model two
// different things. Seen on 172.24.0.210: three cards reading
// "1ded:6001" beside twelve reading "PPU-ZW810E".
func TestAnUnlistedCardTakesTheNameTheCLIGivesItsSiblings(t *testing.T) {
	ppu := func(bdf, driver string) pcicards.Card {
		return pcicards.Card{BDF: bdf, Vendor: accelerator.VendorPPU, VendorID: 0x1ded, DeviceID: 0x6001, Driver: driver, NUMANode: 0}
	}
	cards := []pcicards.Card{
		ppu("0000:08:00.0", pcicards.DriverVFIO),
		ppu("0000:09:00.0", "alipci"),
		ppu("0000:7f:00.0", pcicards.DriverNone),
	}
	listed := []proto.GPUInfo{{Index: 0, UUID: "PPU-09", Name: "PPU-ZW810E", PCIeBDF: "0000:09:00.0", Healthy: true}}
	got := byBDF(mergeRoster(listed, cards, nil, true))
	for _, bdf := range []string{"0000:08:00.0", "0000:7f:00.0"} {
		if got[bdf].Name != "PPU-ZW810E" {
			t.Errorf("%s = %q, want the name its siblings carry", bdf, got[bdf].Name)
		}
	}
	// Ownership is still the card's own: inheriting a name says nothing
	// about who holds it.
	if got["0000:08:00.0"].Ownership != accelerator.CardPassedThrough ||
		got["0000:7f:00.0"].Ownership != accelerator.CardWithoutDriver {
		t.Fatalf("ownership was lost: %+v %+v", got["0000:08:00.0"], got["0000:7f:00.0"])
	}
}

// With no sibling to copy, the databases answer, and their name is used
// as it always was.
func TestWithNoSiblingTheDatabaseNamesTheCard(t *testing.T) {
	cards := []pcicards.Card{{BDF: "0000:1c:00.0", Vendor: accelerator.VendorNVIDIA, VendorID: 0x10de, DeviceID: 0x2204, Driver: "nvidia"}}
	got := mergeRoster(nil, cards, map[string]string{"0000:1c:00.0": "GA102 [GeForce RTX 3090]"}, false)
	if len(got) != 1 || got[0].Name != "GA102 [GeForce RTX 3090]" {
		t.Fatalf("roster = %+v, want the database's name", got)
	}
}

// A sibling of a different model is not a sibling: two models of one
// vendor in a host must not borrow each other's name.
func TestOnlyTheSameModelLendsItsName(t *testing.T) {
	cards := []pcicards.Card{
		{BDF: "0000:41:00.0", Vendor: accelerator.VendorAscend, VendorID: 0x19e5, DeviceID: 0xd802, Driver: "devdrv_device_driver"},
		{BDF: "0000:81:00.0", Vendor: accelerator.VendorAscend, VendorID: 0x19e5, DeviceID: 0xd803, Driver: pcicards.DriverVFIO},
	}
	listed := []proto.GPUInfo{{Index: 0, UUID: "NPU-41", Name: "910B4", PCIeBDF: "0000:41:00.0", Healthy: true}}
	got := byBDF(mergeRoster(listed, cards, nil, true))
	if got["0000:81:00.0"].Name == "910B4" {
		t.Fatal("a d803 took a d802's name")
	}
}
