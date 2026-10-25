// Package pcicards reads which accelerator cards are plugged into this host
// and what holds each one, from sysfs alone.
//
// It exists because the vendor CLIs cannot answer that question. They number
// and list only the cards their driver brought up, so a card whose
// initialization failed is missing from them entirely and the node simply
// looks smaller than it is (zstone-164: nvidia-smi lists 2 of 5 cards bound
// to its driver, and says nothing about the other 3). sysfs is also not
// behind the driver lock, so it answers in milliseconds on a host where
// nvidia-smi takes a minute.
//
// It reads and nothing else: no driver calls, no processes, no judgement
// about health. Deciding what a card's state means belongs to its caller.
package pcicards

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// Driver names sysfs uses for the kernel module bound to a card.
const (
	// DriverVFIO means the card is passed through to a virtual machine; the
	// host cannot use it and must not offer it.
	DriverVFIO = accelerator.PCIDriverVFIO
	// DriverNone means no kernel driver is bound: the card is present but
	// nothing can talk to it until a driver is installed and loaded.
	DriverNone = ""
)

// Card is one accelerator's PCI function as sysfs describes it. Everything
// here is readable without the vendor driver answering.
type Card struct {
	// BDF is the PCI address, normalized (accelerator.NormalizeBDF). It is
	// the card's hardware identity: the slot it sits in.
	BDF string
	// Vendor is accelerator.Vendor*; never empty (a card with an unknown
	// vendor id is not returned at all).
	Vendor string
	// VendorID and DeviceID are the raw PCI ids, kept for naming a model
	// sysfs has no name for.
	VendorID, DeviceID uint32
	// Driver is the bound kernel module: the vendor's own, DriverVFIO, or
	// DriverNone.
	Driver string
	// NUMANode is -1 when sysfs does not say.
	NUMANode int
}

// Unknown is a PCI function of a vendor whose accelerators this platform
// supports, that none of that vendor's rules recognize
// (accelerator.UnknownAcceleratorFunction). It is reported rather than
// dropped so a card the rules do not know about shows up as something an
// operator can send us, instead of as a card that is simply not there.
type Unknown struct {
	BDF                string
	VendorID, DeviceID uint32
	Class              uint32
	Driver             string
}

// Label describes the function the way lspci would, for a report.
func (u Unknown) Label() string {
	return u.BDF + " " + accelerator.PCIDeviceLabel(u.VendorID, u.DeviceID) +
		" class " + accelerator.PCIClassHex(u.Class) + " driver " + driverLabel(u.Driver)
}

func driverLabel(driver string) string {
	if driver == DriverNone {
		return "(none)"
	}
	return driver
}

// Scanner reads a host's PCI devices. The zero value reads this host;
// tests point Root at a fixture tree.
type Scanner struct {
	// Root is prepended to every path read, so a fixture tree can stand in
	// for the host's /sys. Empty means "/".
	Root string
}

// containerMarkers are the files a container runtime leaves behind. A
// container sees the whole host's /sys/bus/pci — measured: a plain
// `docker run alpine` lists all 45 of this machine's PCI functions, with
// vendor ids and driver symlinks readable.
//
// That makes the bus a fact about the host, not about us. An agent in a
// rented pod given one GPU of an eight-GPU machine would otherwise report
// the other seven as this node's cards whose driver failed to bring them
// up: seven phantom faults on a node that is perfectly well.
//
// Docker and Podman leave a file at the root. Kubernetes and plain
// containerd leave none, so those are recognized by what they do leave:
// the service account every pod is given, the variable every pod's
// environment carries, or a container runtime's name in PID 1's cgroup
// path (cgroup v1, and v2 hosts that do not namespace cgroups).
// Some containers expose only the /run/systemd/container marker.
var containerMarkers = []string{
	"/.dockerenv",
	"/run/.containerenv",
	"/var/run/secrets/kubernetes.io/serviceaccount",
	"/run/systemd/container",
}

// containerEnv are environment variables only a container's process has:
// every Kubernetes pod gets the first, and systemd-nspawn, Podman and LXC
// set the second for their PID 1 and its children.
var containerEnv = []string{"KUBERNETES_SERVICE_HOST", "container"}

// procOneCgroup is read for a container runtime's name in PID 1's cgroup.
var procOneCgroup = "/proc/1/cgroup"

var containerCgroupWords = []string{"kubepods", "docker", "containerd", "libpod", "lxc", "crio"}

// hostOwnsThisBus reports whether the PCI bus we can read is ours to
// describe. A fixture root is always the caller's own, so tests are
// unaffected.
func (s Scanner) hostOwnsThisBus() bool {
	if s.Root != "" {
		return true
	}
	for _, marker := range containerMarkers {
		if _, err := os.Stat(marker); err == nil {
			return false
		}
	}
	for _, name := range containerEnv {
		if os.Getenv(name) != "" {
			return false
		}
	}
	if raw, err := os.ReadFile(procOneCgroup); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			// "hierarchy:controllers:path"; only the path names a runtime.
			parts := strings.SplitN(line, ":", 3)
			if len(parts) != 3 {
				continue
			}
			for _, word := range containerCgroupWords {
				if strings.Contains(parts[2], word) {
					return false
				}
			}
		}
	}
	return true
}

// Scan returns this host's accelerator cards and, beside them, the
// functions of a supported vendor that no rule recognized. The second list
// is how a new product line at an unknown class reaches an operator
// instead of disappearing.
//
// Cards come back ordered by PCI address, so the order is stable across
// calls and reboots. An unreadable sysfs returns nothing and no error: a
// host without PCI, or a container that does not map it, simply has
// nothing to say here, and the vendor CLI remains the caller's other
// source.
func (s Scanner) Scan() ([]Card, []Unknown) {
	if !s.hostOwnsThisBus() {
		// Somebody else's bus. The vendor CLI, which is scoped to what
		// this container was given, remains the caller's other source.
		return nil, nil
	}
	dir := s.path("sys/bus/pci/devices")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var cards []Card
	var unknown []Unknown
	for _, entry := range entries {
		card, odd, ok := s.read(dir, entry.Name())
		switch {
		case ok:
			cards = append(cards, card)
		case odd.BDF != "":
			unknown = append(unknown, odd)
		}
	}
	return cards, unknown
}

func (s Scanner) read(dir, name string) (Card, Unknown, bool) {
	device := filepath.Join(dir, name)
	// A virtual function of an SR-IOV card is a slice of a card already
	// counted, not a card of its own.
	if _, err := os.Lstat(filepath.Join(device, "physfn")); err == nil {
		return Card{}, Unknown{}, false
	}
	vendorID, ok := s.hex(device, "vendor")
	if !ok {
		return Card{}, Unknown{}, false
	}
	class, ok := s.hex(device, "class")
	if !ok {
		return Card{}, Unknown{}, false
	}
	bdf := accelerator.NormalizeBDF(name)
	driver := s.driver(device)
	deviceID, _ := s.hex(device, "device")
	vendor := accelerator.VendorForPCI(vendorID, deviceID, class, driver)
	if vendor == "" {
		if accelerator.UnknownAcceleratorFunction(vendorID, deviceID, class, driver) {
			return Card{}, Unknown{BDF: bdf, VendorID: vendorID, DeviceID: deviceID, Class: class, Driver: driver}, false
		}
		return Card{}, Unknown{}, false
	}
	return Card{
		BDF:      bdf,
		Vendor:   vendor,
		VendorID: vendorID,
		DeviceID: deviceID,
		Driver:   driver,
		NUMANode: s.numa(device),
	}, Unknown{}, true
}

// driver is the bound kernel module's name, or DriverNone when the card is
// unbound. sysfs exposes it as a symlink into the driver's directory.
func (s Scanner) driver(device string) string {
	target, err := os.Readlink(filepath.Join(device, "driver"))
	if err != nil {
		return DriverNone
	}
	return filepath.Base(target)
}

func (s Scanner) numa(device string) int {
	raw, err := os.ReadFile(filepath.Join(device, "numa_node"))
	if err != nil {
		return -1
	}
	node, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return -1
	}
	return node
}

// hex reads a sysfs attribute written as "0x10de".
func (s Scanner) hex(device, attribute string) (uint32, bool) {
	raw, err := os.ReadFile(filepath.Join(device, attribute))
	if err != nil {
		return 0, false
	}
	text := strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x")
	value, err := strconv.ParseUint(text, 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(value), true
}

func (s Scanner) path(rel string) string {
	if s.Root == "" {
		return filepath.Join("/", rel)
	}
	return filepath.Join(s.Root, rel)
}
