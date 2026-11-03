package pcicards

// domesticModels are the model names this agent carries itself: the
// accelerators upstream pci.ids has never listed. It has never held a single
// Ascend NPU (of the 72 devices under 19e5, not one is a d8xx), and an
// Ascend host whose npu-smi cannot answer has no other way to say "910B4"
// instead of "19e5:d802". Every other card is named from the host's own
// pci.ids, and a same-machine sibling named by the vendor CLI beats both.
//
// This is the whole of our own model table, kept as small as that rule
// allows: an entry is added only when it was read off real hardware and the
// upstream database lacks it, and removed once upstream has it (a host
// whose pci.ids is that new names the card itself). Each entry says where
// its name came from, because an id named wrong shows somebody else's model
// on every screen and there is no upstream to check against.
//
// A name is written as this platform shows the card when its CLI does
// answer, so one card never reads two ways depending on whether the driver
// was awake: npu-smi says "Ascend950PR" and the Ascend prober makes that
// "950PR" (ascendModel in internal/agent/gpu), so "950PR" is what goes
// here. The two tables are not duplicates: this one maps a PCI device id to
// a name for a card the CLI cannot describe, ascendModel only strips the
// brand off a name the CLI already gave.
var domesticModels = []struct {
	vendor, device uint32
	name           string
	provenance     string
}{
	{0x19e5, 0xd500, "310P3", "npu-smi 24.1.1.3 on 192.0.2.115, 2026-10-17: NPU 4 at bus-id 0000:17:00.0, reported as 310P3; lspci at that address reads 19e5:d500 with no name"},
	{0x19e5, 0xd802, "910B4", "npu-smi on 198.51.100.32, 2026-10-15: NPU 0 and 6, bus-ids 0000:C1:00.0 and 0000:41:00.0"},
	{0x19e5, 0xd803, "910C", "npu-smi 26.1.1 on syl-910c, 2026-10-15: twenty d803 at 81:00.0..9f:00.0, reported as multi-chip Ascend910, which is what this platform calls a 910C (ascend_chips.go)"},
	{0x19e5, 0xd806, "950PR", "npu-smi 25.7.rc1.10, 2026-10-15: one card at bus-id 0000:71:00.0, 128 GiB HBM, reported as Ascend950PR; lspci at that same address reads 19e5:d806"},
	{0x1ded, 0x6001, "PPU-ZW810E", "ppu-smi on 192.0.2.210, 2026-10-15: all twelve cards it serves"},
}

// domesticNames indexes domesticModels by (vendor, device).
func domesticNames() map[[2]uint32]string {
	names := make(map[[2]uint32]string, len(domesticModels))
	for _, m := range domesticModels {
		names[[2]uint32{m.vendor, m.device}] = m.name
	}
	return names
}
