package accelerator

import "testing"

// A card exposes more than its compute function. Measured on gpu3, one
// RTX 2080 Ti presents four: the card at class 0x0300, HD Audio at
// 0x0403, and two USB controllers at 0x0c03 and 0x0c80. Only the first is
// the card; counting the others would treble the node's GPU count.
func TestOnlyTheComputeFunctionOfACardCountsAsAnAccelerator(t *testing.T) {
	const nvidia = 0x10de
	for _, tc := range []struct {
		class uint32
		want  string
		what  string
	}{
		{0x030000, VendorNVIDIA, "the 2080 Ti itself"},
		{0x030200, VendorNVIDIA, "a datacenter card's 3D controller"},
		{0x040300, "", "the card's HD Audio function"},
		{0x0c0330, "", "the card's USB xHCI controller"},
		{0x0c8000, "", "the card's USB Type-C UCSI controller"},
		{0x060400, "", "a PCI bridge"},
	} {
		if got := VendorForPCI(nvidia, 0x2204, tc.class, ""); got != tc.want {
			t.Errorf("class %#06x (%s) = %q, want %q", tc.class, tc.what, got, tc.want)
		}
	}
	// A vendor whose PCI ids this platform has not read off real hardware
	// is not guessed at: its own prober reports its cards.
	if got := VendorForPCI(0x1234, 0x0001, 0x030000, ""); got != "" {
		t.Errorf("an unknown vendor = %q, want no claim", got)
	}
}

// AMD PCI roster identity is established by the existing vendor/driver pair,
// not by a broad display-class rule that could claim unrelated functions.
func TestAMDPCIRosterUsesOnlyTheKnownGPUDriver(t *testing.T) {
	if got := VendorForPCI(0x1002, 0, 0x030000, "amdgpu"); got != VendorAMD {
		t.Fatalf("AMD GPU bound to amdgpu = %q, want %q", got, VendorAMD)
	}
	for _, fn := range []struct {
		class  uint32
		driver string
	}{
		{0x060400, "pcieport"}, {0x040300, "snd_hda_intel"},
		{0x0c0330, "xhci_hcd"}, {0x0c8000, "typec"},
	} {
		if got := VendorForPCI(0x1002, 0, fn.class, fn.driver); got != "" {
			t.Errorf("non-GPU AMD function class=%#06x driver=%q identified as %q", fn.class, fn.driver, got)
		}
	}
	if !HasPCICards(VendorAMD) {
		t.Fatal("HasPCICards(amd) = false")
	}
}

// The display base class is shared with every BMC and integrated graphics
// chip there is, so it is granted per vendor. Huawei's own BMC controller
// carries the same vendor id as its NPUs, and would otherwise add a
// phantom card to every Huawei server.
func TestADisplayFunctionIsOnlyACardForVendorsThatMakeGPUs(t *testing.T) {
	if got := VendorForPCI(0x19e5, 0xd803, 0x030000, "hibmc-drm"); got != "" {
		t.Errorf("Huawei's BMC display controller = %q, want it left out", got)
	}
	if got := VendorForPCI(0x19e5, 0xd803, 0x120000, "devdrv_device_driver"); got != VendorAscend {
		t.Errorf("a 910B4 = %q, want %q", got, VendorAscend)
	}
	// Intel's QAT and DSA sit at the co-processor class, which is Hygon's.
	if got := VendorForPCI(0x8086, 0x0000, 0x0b4000, ""); got != "" {
		t.Errorf("an Intel QAT = %q, want it left out", got)
	}
	if got := VendorForPCI(0x1d94, 0x6211, 0x0b4000, ""); got != VendorHygon {
		t.Errorf("a Hygon DCU = %q, want %q", got, VendorHygon)
	}
}

// The accelerator base class is wholly accelerators, so the whole class
// counts. 0x1201 "AI Inference Accelerator" was added to it after 0x1200
// and must need no change here; the processor base class is not like that,
// as its other subclasses are CPUs.
func TestTheAcceleratorBaseClassCountsWholeAndTheProcessorOneDoesNot(t *testing.T) {
	for _, class := range []uint32{0x120000, 0x120100, 0x127f00} {
		if got := VendorForPCI(0x19e5, 0xd803, class, ""); got != VendorAscend {
			t.Errorf("class %#06x = %q, want %q: the whole base class is accelerators", class, got, VendorAscend)
		}
	}
	for _, class := range []uint32{0x0b0000, 0x0b1000, 0x0b3000} {
		if got := VendorForPCI(0x1d94, 0x6211, class, ""); got != "" {
			t.Errorf("class %#06x = %q, want it left out: that subclass is a CPU", class, got)
		}
	}
}

// A vendor that ships a new product line at a class nobody told us about
// still has its own kernel module bound to it. That is enough.
func TestAVendorsOwnDriverIdentifiesACardWhateverItsClass(t *testing.T) {
	if got := VendorForPCI(0x1ded, 0x6001, 0x138000, "alipci"); got != VendorPPU {
		t.Errorf("a PPU at an unheard-of class, on alipci = %q, want %q", got, VendorPPU)
	}
	if got := VendorForPCI(0x1ded, 0x6001, 0x138000, "vfio-pci"); got != "" {
		t.Errorf("the same function passed through to a VM = %q, want no claim from the class alone", got)
	}
}

// What no rule recognizes is reported rather than dropped, so a card the
// rules do not know about reaches an operator as something to send us.
func TestAnUnrecognizedFunctionOfASupportedVendorIsReported(t *testing.T) {
	if !UnknownAcceleratorFunction(0x1ded, 0x6001, 0x0b8000, "") {
		t.Error("an unbound PPU-vendor function at a processor subclass was dropped silently")
	}
	// Recognized ones are not reported: they are cards.
	if UnknownAcceleratorFunction(0x1ded, 0x6001, 0x030200, "alipci") {
		t.Error("a PPU was reported as unrecognized")
	}
	// Not Huawei's BMC display controller: every one of its servers has
	// one, and a permanent warning on every Ascend node is worse than
	// useless.
	if UnknownAcceleratorFunction(0x19e5, 0xd803, 0x030000, "hibmc-drm") {
		t.Error("the BMC display controller would warn on every Huawei server")
	}
	// Nor the rest of a Huawei server, which carries the same vendor id:
	// bridges, SAS and SATA, USB, NICs, the DMA engine. Measured on
	// 198.51.100.31, where reporting them printed eighteen lines.
	for _, fn := range []struct {
		class  uint32
		driver string
		what   string
	}{
		{0x060400, "pcieport", "a PCI bridge"},
		{0x010700, "hisi_sas_v3_hw", "a SAS controller"},
		{0x010601, "ahci", "a SATA controller"},
		{0x0c0330, "xhci_hcd", "a USB host controller"},
		{0x020000, "hns3", "a network interface"},
		{0x118000, "edma_drv", "the DMA engine"},
	} {
		if UnknownAcceleratorFunction(0x19e5, 0xd803, fn.class, fn.driver) {
			t.Errorf("%s was reported as a possible accelerator", fn.what)
		}
	}
	// Nor the SoC's own compression engine, which reports the accelerator
	// class the NPU reports. A 910C host has eight of them, and neither
	// listing them as cards nor warning about them helps anyone.
	if got := VendorForPCI(0x19e5, 0xa250, 0x120000, "hisi_zip"); got != "" {
		t.Errorf("the ZIP engine was identified as an accelerator of %q", got)
	}
	if UnknownAcceleratorFunction(0x19e5, 0xa250, 0x120000, "hisi_zip") {
		t.Error("the ZIP engine would warn on every Kunpeng server")
	}
	// Unbound, it is still the same engine.
	if got := VendorForPCI(0x19e5, 0xa250, 0x120000, ""); got != "" {
		t.Errorf("an unbound ZIP engine was identified as an accelerator of %q", got)
	}
	// But a base class PCI-SIG has not assigned is exactly the case this
	// exists for, and is reported.
	if !UnknownAcceleratorFunction(0x19e5, 0xd803, 0x200000, "some_new_module") {
		t.Error("a function at an unassigned base class was dropped silently")
	}
	// A vendor we do not support at all is none of our business.
	if UnknownAcceleratorFunction(0x8086, 0x0000, 0x0b4000, "") {
		t.Error("an Intel QAT was reported as an unrecognized accelerator")
	}
	// Neither is anything handed to a virtual machine: a host passes all
	// sorts of hardware through, and a vendor id alone proves nothing.
	if UnknownAcceleratorFunction(0x1ded, 0x6001, 0x0b8000, "vfio-pci") {
		t.Error("a passed-through function was reported as an unrecognized accelerator")
	}
}

// A card with no name in pci.ids is still named, the way lspci prints it,
// with both ids padded to four hex digits.
func TestAnUnnamedCardIsLabelledByItsIDs(t *testing.T) {
	if got := PCIDeviceLabel(0x10de, 0x2204); got != "10de:2204" {
		t.Errorf("label = %q", got)
	}
	if got := PCIDeviceLabel(0x1, 0x20); got != "0001:0020" {
		t.Errorf("short ids = %q, want them padded as sysfs spells them", got)
	}
	if got := PCIVendorHex(0x10de); got != "10de" {
		t.Errorf("vendor hex = %q", got)
	}
	if got := PCIClassHex(0x030200); got != "0302" {
		t.Errorf("class hex = %q, want it as lspci prints it", got)
	}
}

// The vendors a node can be told to manage are those a PCI bus lists:
// every one but Apple, and nothing unknown.
func TestOnlyVendorsOnAPCIBusCanBeChosen(t *testing.T) {
	for _, vendor := range []string{VendorNVIDIA, VendorAscend, VendorHygon, VendorPPU, VendorAMD} {
		if !HasPCICards(vendor) {
			t.Fatalf("HasPCICards(%q) = false", vendor)
		}
	}
	for _, vendor := range []string{VendorApple, "", "auto", "NVIDIA"} {
		if HasPCICards(vendor) {
			t.Fatalf("HasPCICards(%q) = true", vendor)
		}
	}
}
