package gpu

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu/pcicards"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A node manages one vendor's cards. On a host with cards of more than one,
// New picks a vendor in its own order and the operator may choose another;
// the choice is a vendor name, or proto.AcceleratorPreferenceAuto for New's.

// NewFor returns the backend that manages the chosen vendor's cards, or
// New's pick for an automatic choice. A vendor the operator chose is kept
// even when its CLI does not answer: its probe failure is then reported for
// what it is, rather than the node turning into another vendor's.
// A vendor this build has no backend for is treated as automatic.
func NewFor(preference string) Prober {
	if p := chosenProber(defaultProbers(), preference); p != nil {
		return p
	}
	return New()
}

func chosenProber(probers []Prober, preference string) Prober {
	if !ChoosableVendor(preference) {
		return nil
	}
	for _, p := range probers {
		if p.Vendor() == preference {
			return p
		}
	}
	return nil
}

// ChoosableVendor reports whether an operator may choose to manage this
// vendor's cards: a vendor whose cards a PCI bus can show, which is every
// backend but Apple's.
func ChoosableVendor(vendor string) bool { return accelerator.HasPCICards(vendor) }

// VendorCensus lists every vendor with cards on the host's PCI bus, in
// vendor order, for the operator to choose from. A card passed through to a
// virtual machine is not the host's to use, and a vendor with only such
// cards is left out. A bus that cannot be read, as in a container, lists
// nothing. Reading the bus is a directory walk; naming the cards reads
// pci.ids, megabytes long, so names are looked up again only when the
// cards change.
type VendorCensus struct {
	// HostRoot is where the host's /sys is mounted ("" for the host itself).
	HostRoot string

	mu   sync.Mutex
	key  string
	last []proto.AcceleratorPresence
}

// Vendors is the census as the bus has it now.
func (c *VendorCensus) Vendors() []proto.AcceleratorPresence {
	scanner := pcicards.Scanner{Root: c.HostRoot}
	cards, _ := scanner.Scan()
	byVendor := map[string][]pcicards.Card{}
	var keys []string
	for _, card := range cards {
		if card.Driver == pcicards.DriverVFIO || !ChoosableVendor(card.Vendor) {
			continue
		}
		byVendor[card.Vendor] = append(byVendor[card.Vendor], card)
		keys = append(keys, fmt.Sprintf("%s/%s/%x:%x", card.BDF, card.Vendor, card.VendorID, card.DeviceID))
	}
	sort.Strings(keys)
	key := strings.Join(keys, ",")
	c.mu.Lock()
	defer c.mu.Unlock()
	if key == c.key && c.last != nil {
		return append([]proto.AcceleratorPresence(nil), c.last...)
	}
	out := make([]proto.AcceleratorPresence, 0, len(byVendor))
	for vendor, own := range byVendor {
		out = append(out, proto.AcceleratorPresence{Vendor: vendor, Cards: len(own), Model: commonModel(own, scanner.ModelNames(own))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Vendor < out[j].Vendor })
	c.key, c.last = key, out
	if len(out) == 0 {
		return nil
	}
	return append([]proto.AcceleratorPresence(nil), out...)
}

// commonModel is the model most of the cards are, by name, ties going to
// the name that sorts first so the answer does not flicker between reports.
func commonModel(cards []pcicards.Card, names map[string]string) string {
	counts := map[string]int{}
	for _, card := range cards {
		if name := strings.TrimSpace(names[card.BDF]); name != "" {
			counts[name]++
		}
	}
	best := ""
	for name, n := range counts {
		if n > counts[best] || (n == counts[best] && name < best) {
			best = name
		}
	}
	return best
}
