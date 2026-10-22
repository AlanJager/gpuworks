package proto

// AcceleratorPreferenceAuto is the accelerator choice that lets the agent
// pick, in its own order, which vendor's cards a node manages. Any other
// choice is a vendor name (nvidia, ascend, ...). A node manages one vendor's
// cards at a time; the choice matters only on a host that has cards of more
// than one, which the agent reports as SysInfo.AcceleratorVendors.
const AcceleratorPreferenceAuto = "auto"

// AcceleratorPresence is one vendor whose cards are in the host, as its PCI
// bus lists them, whether or not the node manages them. Cards counts the
// ones the host can use (not passed through to a virtual machine); Model is
// the most common card model among them, empty when pci.ids has no name.
type AcceleratorPresence struct {
	Vendor string `json:"vendor"`
	Cards  int    `json:"cards"`
	Model  string `json:"model,omitempty"`
}
