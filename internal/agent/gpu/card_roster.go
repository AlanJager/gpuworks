package gpu

import (
	"log/slog"
	"sort"
	"sync"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu/pcicards"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Which cards a node has, against what the driver can tell us about them.
//
// These are two different questions and only one of them is the driver's to
// answer. The cards plugged in are a fact of the hardware, readable from
// sysfs in milliseconds whatever state the driver is in; what each card is
// (its memory, its UUID, its health) only the driver knows. Asking the
// driver for the roster is what made a card whose initialization failed
// disappear from the node entirely — zstone-164 reported 2 cards of the 8
// in it, with no sign that the other 6 existed.
//
// So the roster comes from PCI, and the driver's answer is merged onto it.
// A card the driver does not mention keeps only what PCI knows, plus why
// that is: it is passed through, it has no driver, or its driver has it and
// could not bring it up.

// mergeRoster returns every accelerator the host has, with the driver's
// facts merged onto the cards it listed. listed is what the vendor CLI
// reported; answered says whether the CLI answered at all, which is what
// separates "the driver could not bring this card up" from "we could not
// ask"; cards is the PCI roster; names is the model name of each card
// where pci.ids has one.
//
// It is pure, so the merge rule itself is tested without a host.
func mergeRoster(listed []proto.GPUInfo, cards []pcicards.Card, names map[string]string, answered bool) []proto.GPUInfo {
	byBDF := make(map[string]proto.GPUInfo, len(listed))
	// A card the CLI listed without a PCI address cannot be matched to the
	// bus, and one whose address it repeats can only be matched once.
	// Neither may be lost: Ascend and Hygon both read the address out of a
	// table row and can come back without one, and dropping those cards
	// replaced two real 910B4 — UUID, memory and all — with two PCI
	// phantoms that said their driver had failed.
	var unmatchable []proto.GPUInfo
	for _, gpu := range listed {
		// One spelling of the address for every card, whatever the tool
		// printed: the stored card is matched to the PCI bus by it, and a
		// card written one way by a full probe and the other by a
		// PCI-only one was rewritten every time the two alternated.
		bdf := accelerator.NormalizeBDF(gpu.PCIeBDF)
		gpu.PCIeBDF = bdf
		if _, taken := byBDF[bdf]; bdf == "" || taken {
			unmatchable = append(unmatchable, gpu)
			continue
		}
		byBDF[bdf] = gpu
	}
	siblings := siblingNames(byBDF, cards)
	roster := make([]proto.GPUInfo, 0, len(cards))
	for _, card := range cards {
		if gpu, ok := byBDF[card.BDF]; ok {
			// The driver has this card and answered for it. Its facts win;
			// PCI only fills what the CLI leaves blank.
			delete(byBDF, card.BDF)
			if gpu.NUMANode <= 0 && card.NUMANode >= 0 {
				gpu.NUMANode = card.NUMANode
			}
			roster = append(roster, gpu)
			continue
		}
		roster = append(roster, unlistedCard(card, pickName(card, siblings, names), answered))
	}
	// A card the CLI listed but PCI did not: a container that maps the
	// device without mapping PCI sysfs, or a vendor this host enumerates
	// only through its CLI. The CLI's answer is all there is, and it is
	// trusted as before.
	remaining := make([]proto.GPUInfo, 0, len(byBDF)+len(unmatchable))
	for _, gpu := range byBDF {
		remaining = append(remaining, gpu)
	}
	remaining = append(remaining, unmatchable...)
	sort.Slice(remaining, func(i, j int) bool { return remaining[i].Index < remaining[j].Index })
	roster = append(roster, remaining...)
	return roster
}

// siblingNames is the name the vendor CLI gave to a card of each model on
// this host, by PCI ids.
//
// A host rarely has one card of a kind. When some of them are passed
// through to a virtual machine and the rest are not, the CLI names the
// rest — and that name is the same model's name, from the vendor's own
// tool, on this very machine. It beats any database: it needs no table to
// have heard of the card, and it is the name the platform already shows
// for the card's siblings, so one list does not call the same model two
// different things (seen on the PPU host: three cards reading
// "1ded:6001" beside twelve reading "PPU-ZW810E").
func siblingNames(byBDF map[string]proto.GPUInfo, cards []pcicards.Card) map[[2]uint32]string {
	named := map[[2]uint32]string{}
	for _, card := range cards {
		gpu, ok := byBDF[card.BDF]
		if !ok || gpu.Name == "" {
			continue
		}
		named[[2]uint32{card.VendorID, card.DeviceID}] = gpu.Name
	}
	return named
}

// pickName is what to call a card the CLI did not list: what the CLI
// calls this model elsewhere on the host, else what the id databases say
// (pcicards.ModelNames), else its ids.
func pickName(card pcicards.Card, siblings map[[2]uint32]string, names map[string]string) string {
	if name := siblings[[2]uint32{card.VendorID, card.DeviceID}]; name != "" {
		return name
	}
	return names[card.BDF]
}

// unlistedCard describes a card the vendor CLI did not list, and says why.
// There is no index to give it: an index is what the CLI hands out.
func unlistedCard(card pcicards.Card, name string, answered bool) proto.GPUInfo {
	if name == "" {
		name = accelerator.PCIDeviceLabel(card.VendorID, card.DeviceID)
	}
	gpu := proto.GPUInfo{
		Index:    -1,
		Name:     name,
		Vendor:   card.Vendor,
		PCIeBDF:  card.BDF,
		NUMANode: card.NUMANode,
	}
	switch {
	case card.Driver == pcicards.DriverVFIO:
		// Assigned to a virtual machine. Nothing is wrong with it.
		gpu.Ownership = accelerator.CardPassedThrough
	case card.Driver == pcicards.DriverNone:
		gpu.Ownership = accelerator.CardWithoutDriver
	case !accelerator.IsVendorAcceleratorDriver(card.VendorID, card.Driver):
		// Something else holds the card — nouveau, pci-stub, a hypervisor's
		// passthrough stub. The vendor's stack cannot reach it either way,
		// so it is not this node's card and certainly not a fault of it.
		gpu.Ownership = accelerator.CardHeldElsewhere
	default:
		// The vendor's own driver holds the card. Whether that is the
		// card's fault depends on whether the CLI answered: one that
		// answered and left this card out could not bring it up, while
		// one that did not answer at all says nothing about the card.
		gpu.Ownership = accelerator.CardOwnedByNode
		code, cliEvidence := accelerator.HealthDriverInitFailed, "does not list this card"
		if !answered {
			code, cliEvidence = accelerator.HealthCLIUnanswered, "did not answer"
		}
		SetCardFinding(&gpu, code, []accelerator.HealthField{
			{Name: "PCI Address", Value: card.BDF},
			{Name: "Bound Driver", Value: card.Driver},
			{Name: "Vendor CLI", Value: cliEvidence},
		})
		return gpu
	}
	gpu.Healthy = CardIsHealthy(gpu)
	return gpu
}

// Roster is one vendor's card list: every card of that vendor the host has
// on its PCI bus, with whatever the vendor CLI could add about each.
// answered says whether that CLI answered this round, which separates "the
// driver could not bring this card up" from "we could not ask".
//
// hostRoot is where the host's /sys is mounted ("" for the host itself). A
// host whose PCI sysfs cannot be read — a container that does not map it —
// falls back to the CLI's own list, which is how every prober behaved
// before this existed.
//
// Every vendor's prober calls this. The question "which cards are in this
// machine" has the same answer and the same source for all of them, and
// the cards a CLI leaves out are the ones an operator most needs to see:
// measured on 2026-10-15, an Ascend host with eight 910B4 in it reported
// as having no accelerator at all, because npu-smi could not initialize.
func Roster(vendor, hostRoot string, listed []proto.GPUInfo, answered bool) []proto.GPUInfo {
	scanner := pcicards.Scanner{Root: hostRoot}
	cards, _ := scanner.Scan()
	own := ownCards(cards, vendor)
	if len(own) == 0 {
		return listed
	}
	roster := mergeRoster(listed, own, scanner.ModelNames(own), answered)
	unlisted.report(vendor, len(listed), len(roster))
	return roster
}

// unlisted logs when the cards the vendor CLI leaves out change, not every
// time a probe finds them again: a host with cards passed through to its
// virtual machines has them every minute, from every caller that probes.
var unlisted unlistedLog

type unlistedLog struct {
	mu   sync.Mutex
	last map[string][2]int
}

func (l *unlistedLog) report(vendor string, listed, present int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string][2]int{}
	}
	now := [2]int{listed, present}
	if l.last[vendor] == now {
		return
	}
	l.last[vendor] = now
	if present > listed {
		slog.Info("accelerators present that the vendor CLI does not list",
			"vendor", vendor, "listed", listed, "present", present)
	}
}

// UnrecognizedAccelerators are the PCI functions of a supported vendor that
// no rule recognized, which the agent reports so a card the rules do not
// know about reaches an operator with the ids needed to add the rule.
func UnrecognizedAccelerators(hostRoot string) []pcicards.Unknown {
	_, unknown := pcicards.Scanner{Root: hostRoot}.Scan()
	return unknown
}

// ownCards is one vendor's cards out of a scan: another vendor's prober
// reports its own.
func ownCards(cards []pcicards.Card, vendor string) []pcicards.Card {
	own := make([]pcicards.Card, 0, len(cards))
	for _, card := range cards {
		if card.Vendor == vendor {
			own = append(own, card)
		}
	}
	return own
}

// CLIHasNothingToAnswer reports whether the vendor CLI failing says
// nothing about this node: the bus has cards of its vendor and not one of
// them is the node's to use — every one is passed through to a virtual
// machine, bound to no driver, or held by a driver that is not the
// vendor's. The CLI has no card to describe, so its exit status is the
// node's steady state rather than a failed reading.
//
// Measured on a Hygon host whose only K100_AI is passed through
// (192.0.2.133, 2026-10-15): hy-smi exits 1 every minute, and before
// this the node carried a permanent "the readings are out of date"
// warning over a card list that was exactly current — it comes from the
// PCI bus, which answered.
//
// An empty list is not this case: a host whose PCI sysfs could not be read
// falls back to the CLI's own list, and there a failure means the cards
// really are unknown.
//
// The status report and doctor both ask it, so a node does not read as
// failing in one and fine in the other.
func CLIHasNothingToAnswer(gpus []proto.GPUInfo) bool {
	if len(gpus) == 0 {
		return false
	}
	for _, g := range gpus {
		if accelerator.CardUsable(g.Ownership) {
			return false
		}
	}
	return true
}
