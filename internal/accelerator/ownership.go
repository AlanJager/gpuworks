package accelerator

// Whose a card is. A node reports every accelerator plugged into it,
// because that is a fact an operator can see by opening the case, but only
// some of them are the node's to run models on: a card handed to a virtual
// machine, or one no driver has claimed, is present and unusable.
//
// This is deliberately not expressed as health. A passed-through card is
// not broken, and saying so would send somebody to look for a fault that
// is not there.
const (
	// CardOwnedByNode is a card this node may use. It is the empty string
	// so that an agent predating ownership reads as usable, which is what
	// every card it reports is.
	CardOwnedByNode = ""
	// CardPassedThrough is bound to a VFIO driver: assigned to a virtual
	// machine, which may be on this very host. The host cannot touch it.
	CardPassedThrough = "passthrough"
	// CardWithoutDriver has no kernel driver bound. The hardware is there
	// and nothing can talk to it until a driver is installed and loaded.
	CardWithoutDriver = "no_driver"
	// CardHeldElsewhere is bound to a driver that is neither the vendor's
	// own nor VFIO: nouveau on an NVIDIA card, pci-stub, a hypervisor's
	// passthrough stub. The vendor's stack cannot reach it, so it is not
	// this node's to use — and not a fault of the card either.
	CardHeldElsewhere = "held_elsewhere"
)

// CardUsable reports whether a card's ownership lets this node run work on
// it. It says nothing about the card's health, which is a separate question
// asked of a card this node does own.
func CardUsable(ownership string) bool { return ownership == CardOwnedByNode }
