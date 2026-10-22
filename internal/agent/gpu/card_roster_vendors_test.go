package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// pciHost writes a sysfs tree of PCI functions, as the kernel lays it out.
// A driver of "" leaves the card unbound.
type pciFunction struct{ bdf, vendor, device, class, driver string }

func pciHost(t *testing.T, functions ...pciFunction) string {
	t.Helper()
	root := t.TempDir()
	for _, fn := range functions {
		device := filepath.Join(root, "sys/bus/pci/devices", fn.bdf)
		if err := os.MkdirAll(device, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{
			"vendor": fn.vendor, "device": fn.device, "class": fn.class, "numa_node": "0",
		} {
			if err := os.WriteFile(filepath.Join(device, name), []byte(value+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if fn.driver != "" {
			if err := os.Symlink("../../../../bus/pci/drivers/"+fn.driver, filepath.Join(device, "driver")); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// 172.30.8.31 as measured on 2026-10-15: eight 910B4, four bound to the
// Huawei driver and four handed to virtual machines, plus the BMC display
// controller that shares Huawei's PCI vendor id.
func ascendHost31(t *testing.T) string {
	t.Helper()
	npu := func(bdf, driver string) pciFunction {
		return pciFunction{bdf: bdf, vendor: "0x19e5", device: "0xd802", class: "0x120000", driver: driver}
	}
	return pciHost(t,
		npu("0000:01:00.0", "devdrv_device_driver"),
		npu("0000:02:00.0", "vfio-pci"),
		npu("0000:41:00.0", "vfio-pci"),
		npu("0000:42:00.0", "devdrv_device_driver"),
		npu("0000:81:00.0", "devdrv_device_driver"),
		npu("0000:82:00.0", "vfio-pci"),
		npu("0000:c1:00.0", "vfio-pci"),
		npu("0000:c2:00.0", "devdrv_device_driver"),
		pciFunction{bdf: "0000:03:00.0", vendor: "0x19e5", device: "0x1711", class: "0x030000", driver: "hibmc-drm"},
	)
}

// An Ascend host whose npu-smi cannot initialize reports eight cards, not
// none. Measured on 172.30.8.31, where "npu-smi info" answers "dcmi module
// initialize failed. ret is -8001" and the platform showed the machine as
// having no accelerator at all.
func TestAscendReportsItsCardsWhenNPUSMICannotBeAsked(t *testing.T) {
	p := &AscendProber{hostRoot: ascendHost31(t), runner: func(context.Context, ...string) (string, error) {
		return "", errDCMI
	}}
	cards, err := p.Probe(context.Background())
	if err == nil {
		t.Fatal("a failed npu-smi reported no error")
	}
	if len(cards) != 8 {
		t.Fatalf("reported %d cards, want the 8 the machine has: %+v", len(cards), cards)
	}
	var owned, passthrough int
	for _, card := range cards {
		if card.Vendor != accelerator.VendorAscend {
			t.Fatalf("card %s = vendor %q", card.PCIeBDF, card.Vendor)
		}
		switch card.Ownership {
		case accelerator.CardOwnedByNode:
			owned++
			if card.HealthCode != accelerator.HealthCLIUnanswered {
				t.Fatalf("%s = %q, want it to say the CLI did not answer", card.PCIeBDF, card.HealthCode)
			}
		case accelerator.CardPassedThrough:
			passthrough++
			if card.HealthCode != "" {
				t.Fatalf("a passed-through card %s carries a fault: %q", card.PCIeBDF, card.HealthCode)
			}
		}
		if card.Healthy {
			t.Fatalf("%s reads as healthy although nothing could be asked about it", card.PCIeBDF)
		}
	}
	if owned != 4 || passthrough != 4 {
		t.Fatalf("owned=%d passthrough=%d, want 4 and 4", owned, passthrough)
	}
}

// The BMC display controller shares Huawei's vendor id and is not a card.
// Reporting it would add a phantom NPU to every Huawei server.
func TestTheBMCDisplayControllerIsNotAnAscendCard(t *testing.T) {
	root := pciHost(t, pciFunction{bdf: "0000:03:00.0", vendor: "0x19e5", device: "0x1711", class: "0x030000", driver: "hibmc-drm"})
	if cards := Roster(accelerator.VendorAscend, root, nil, false); len(cards) != 0 {
		t.Fatalf("roster = %+v, want the server's out-of-band console left out", cards)
	}
}

// 172.24.0.210 as measured: sixteen PPU ZW810E, twelve on the vendor
// driver, three passed through and one unbound. ppu-smi lists the twelve.
func TestPPUListsThePassedThroughAndUnboundCardsBesideTheTwelveItServes(t *testing.T) {
	ppu := func(bdf, driver string) pciFunction {
		return pciFunction{bdf: bdf, vendor: "0x1ded", device: "0x6001", class: "0x030200", driver: driver}
	}
	root := pciHost(t,
		ppu("0000:08:00.0", "vfio-pci"), ppu("0000:09:00.0", "alipci"),
		ppu("0000:7e:00.0", "alipci"), ppu("0000:7f:00.0", ""),
		ppu("0000:a2:00.0", "alipci"), ppu("0000:a3:00.0", "alipci"),
		// Intel QAT and DSA sit at the same class as a Hygon DCU.
		pciFunction{bdf: "0000:ed:00.0", vendor: "0x8086", device: "0x4940", class: "0x0b4000"},
	)
	listed := []proto.GPUInfo{
		{Index: 0, UUID: "PPU-09", Name: "PPU-ZW810E", PCIeBDF: "0000:09:00.0", Healthy: true},
		{Index: 1, UUID: "PPU-7e", Name: "PPU-ZW810E", PCIeBDF: "0000:7e:00.0", Healthy: true},
		{Index: 2, UUID: "PPU-a2", Name: "PPU-ZW810E", PCIeBDF: "0000:a2:00.0", Healthy: true},
		{Index: 3, UUID: "PPU-a3", Name: "PPU-ZW810E", PCIeBDF: "0000:a3:00.0", Healthy: true},
	}
	cards := Roster(accelerator.VendorPPU, root, listed, true)
	if len(cards) != 6 {
		t.Fatalf("roster = %d cards, want the 6 PPUs and no Intel accelerator: %+v", len(cards), cards)
	}
	got := byBDF(cards)
	if got["0000:08:00.0"].Ownership != accelerator.CardPassedThrough {
		t.Fatalf("the card given to a VM = %+v", got["0000:08:00.0"])
	}
	if got["0000:7f:00.0"].Ownership != accelerator.CardWithoutDriver {
		t.Fatalf("the unbound card = %+v", got["0000:7f:00.0"])
	}
	// ppu-smi answered, so the cards it serves keep its facts untouched.
	if got["0000:09:00.0"].UUID != "PPU-09" || !got["0000:09:00.0"].Healthy {
		t.Fatalf("a card ppu-smi serves = %+v", got["0000:09:00.0"])
	}
}

// 172.25.17.133: the host's only DCU is passed through to a virtual
// machine, so hy-smi answers "No device available". Saying "this host has
// no accelerator" sends an operator looking for hardware that is sitting
// in the machine; "one card, passed through" is the answer.
func TestHygonSaysItsOnlyCardIsPassedThroughRatherThanAbsent(t *testing.T) {
	root := pciHost(t, pciFunction{bdf: "0000:67:00.0", vendor: "0x1d94", device: "0x6211", class: "0x0b4000", driver: "vfio-pci"})
	// Upstream pci.ids knows this model, so the name comes from the host's
	// copy of it.
	ids := filepath.Join(root, "usr/share/hwdata/pci.ids")
	if err := os.MkdirAll(filepath.Dir(ids), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ids, []byte("1d94  Chengdu Haiguang IC Design Co., Ltd.\n\t6211  K100_AI\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &HygonProber{
		hostRoot: root,
		lookPath: func(string) (string, error) { return "/usr/local/bin/hy-smi", nil },
		command:  func(context.Context, ...string) (string, error) { return "", errNoDevice },
	}
	cards, err := p.Probe(context.Background())
	if err == nil {
		t.Fatal("hy-smi failing reported no error")
	}
	if len(cards) != 1 {
		t.Fatalf("reported %d cards, want the one in the machine: %+v", len(cards), cards)
	}
	if cards[0].Ownership != accelerator.CardPassedThrough || cards[0].HealthCode != "" {
		t.Fatalf("card = %+v, want it marked as passed through and not as a fault", cards[0])
	}
	// The card reads as what it is even though hy-smi could not be asked.
	if cards[0].Name != "K100_AI" {
		t.Fatalf("name = %q, want the carried model name", cards[0].Name)
	}
}

var (
	errDCMI     = errTest("dcmi module initialize failed. ret is -8001")
	errNoDevice = errTest("No device available, no device found or initialization failed, exiting.")
)

type errTest string

func (e errTest) Error() string { return string(e) }

// A vendor that ships a new product line at a class nobody announced
// still binds its own kernel module to it, and that is enough to list the
// card. This is the rule that keeps the class table from having to be
// exhaustive, which it cannot be.
func TestACardOnItsVendorsDriverIsListedWhateverItsClass(t *testing.T) {
	root := pciHost(t,
		pciFunction{bdf: "0000:41:00.0", vendor: "0x19e5", device: "0xd900", class: "0x138000", driver: "devdrv_device_driver"},
	)
	cards := Roster(accelerator.VendorAscend, root, nil, false)
	if len(cards) != 1 {
		t.Fatalf("roster = %+v, want the card its own driver holds", cards)
	}
	if cards[0].Ownership != accelerator.CardOwnedByNode || cards[0].HealthCode != accelerator.HealthCLIUnanswered {
		t.Fatalf("card = %+v", cards[0])
	}
}

// What no rule recognizes is handed back to be reported, with the ids a
// rule would need. Silence here is a card missing from its node.
func TestAnUnrecognizedFunctionIsHandedBackRatherThanDropped(t *testing.T) {
	root := pciHost(t,
		pciFunction{bdf: "0000:41:00.0", vendor: "0x19e5", device: "0xd802", class: "0x120000", driver: "devdrv_device_driver"},
		// A base class PCI-SIG has not assigned: nobody will tell us when
		// a vendor starts using one, so it is reported rather than dropped.
		pciFunction{bdf: "0000:51:00.0", vendor: "0x19e5", device: "0xdb00", class: "0x200000", driver: "some_new_module"},
		// Neither a supported vendor nor a card: nobody's business.
		pciFunction{bdf: "0000:ed:00.0", vendor: "0x8086", device: "0x4940", class: "0x0b4000"},
	)
	cards, unknown := Roster(accelerator.VendorAscend, root, nil, false), UnrecognizedAccelerators(root)
	if len(cards) != 1 || cards[0].PCIeBDF != "0000:41:00.0" {
		t.Fatalf("cards = %+v, want the one card the rules know", cards)
	}
	if len(unknown) != 1 || unknown[0].BDF != "0000:51:00.0" {
		t.Fatalf("unknown = %+v, want the one function no rule covers", unknown)
	}
	label := unknown[0].Label()
	for _, want := range []string{"0000:51:00.0", "19e5:db00", "2000", "some_new_module"} {
		if !strings.Contains(label, want) {
			t.Fatalf("report %q does not carry %q, which a rule would need", label, want)
		}
	}
}

// The roster only ever adds. A card the vendor CLI listed is kept with
// the CLI's own facts whether or not the PCI table knows its model, so a
// model the table has not been taught — or a whole vendor it has not —
// still reaches the platform and is schedulable.
//
// This is what lets the table be wrong without costing anybody a working
// card: its failure mode is a passed-through or broken card going
// unreported, never a usable one disappearing.
func TestACardTheCLIListsIsNeverDroppedByTheRoster(t *testing.T) {
	newModel := proto.GPUInfo{
		Index: 0, UUID: "GPU-new", Name: "A model the table has never heard of",
		PCIeBDF: "0000:5e:00.0", MemoryTotalBytes: 141 << 30, Healthy: true,
	}
	for _, tc := range []struct {
		what string
		root string
	}{
		// Nothing of this vendor on the bus that any rule matches.
		{"a host whose PCI says nothing about this vendor", pciHost(t,
			pciFunction{bdf: "0000:5e:00.0", vendor: "0x19e5", device: "0xe000", class: "0x0c0330", driver: "xhci_hcd"})},
		// PCI knows some of the vendor's cards but not this one.
		{"a host where the table knows a sibling card", pciHost(t,
			pciFunction{bdf: "0000:41:00.0", vendor: "0x19e5", device: "0xd802", class: "0x120000", driver: "devdrv_device_driver"})},
		// No readable sysfs at all: a container that does not map it.
		{"a container with no PCI sysfs", t.TempDir()},
	} {
		cards := Roster(accelerator.VendorAscend, tc.root, []proto.GPUInfo{newModel}, true)
		var found *proto.GPUInfo
		for i := range cards {
			if cards[i].UUID == newModel.UUID {
				found = &cards[i]
			}
		}
		if found == nil {
			t.Fatalf("%s: the CLI's card is gone from %+v", tc.what, cards)
		}
		if found.Name != newModel.Name || found.MemoryTotalBytes != newModel.MemoryTotalBytes {
			t.Fatalf("%s: the CLI's facts were overwritten: %+v", tc.what, *found)
		}
		if found.Ownership != accelerator.CardOwnedByNode || !found.Healthy {
			t.Fatalf("%s: card = %+v, want it this node's and usable", tc.what, *found)
		}
	}
}
