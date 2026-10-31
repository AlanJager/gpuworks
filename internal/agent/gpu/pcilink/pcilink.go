// Package pcilink reads, from sysfs alone, how an accelerator is connected to
// the host: the PCIe links between it and its root port, each link's
// negotiated and capable width and speed, and the AER error counters of the
// devices on the way. It starts no process and knows nothing of the agent.
package pcilink

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// PCIe device/port types (PCI Express Capabilities register, bits 7:4).
const (
	PortEndpoint       = 0
	PortLegacyEndpoint = 1
	PortRoot           = 4
	PortUpstream       = 5
	PortDownstream     = 6
	PortPCIeToPCI      = 7
	PortUnknown        = -1
)

// Link is one PCIe link, as seen from the device below it: the card itself,
// or a switch's upstream port. A root port or a switch's downstream port
// describes the link below it, which the device below describes too, so they
// are not links of their own here — that is also what keeps a switch's
// internal (virtual) links out.
type Link struct {
	// BDF is the device whose upstream link this is; Parent the port above
	// it, "" when the parent is not a PCI device (a root complex).
	BDF, Parent string
	// Width and Speed are what the link negotiated; Speed in GT/s.
	Width int
	Speed float64
	// ExpectedWidth and ExpectedSpeed are the most both ends can do: the
	// smaller of the two ends' maximums.
	ExpectedWidth int
	ExpectedSpeed float64
}

// Narrow reports a link running at fewer lanes than both ends support.
func (l Link) Narrow() bool { return l.ExpectedWidth > 0 && l.Width < l.ExpectedWidth }

// Slow reports a link running below the speed both ends support. An idle card
// is allowed to: GPUs lower their link speed to save power.
func (l Link) Slow() bool { return l.ExpectedSpeed > 0 && l.Speed+0.01 < l.ExpectedSpeed }

// AER is one device's Advanced Error Reporting counters since boot.
type AER struct {
	Correctable, NonFatal, Fatal int64
}

// Reader reads one host's PCI tree. Root is where the host's / is ("" for
// "/"); a test points it at a fixture.
type Reader struct{ Root string }

var bdfRE = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

func (r Reader) path(rel string) string {
	root := r.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, rel)
}

// Chain is the PCI devices from the top of the hierarchy down to bdf.
func (r Reader) Chain(bdf string) ([]string, error) {
	dev := r.path(filepath.Join("sys/bus/pci/devices", bdf))
	resolved, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return nil, err
	}
	var chain []string
	for _, part := range strings.Split(resolved, string(filepath.Separator)) {
		if bdfRE.MatchString(part) {
			chain = append(chain, strings.ToLower(part))
		}
	}
	return chain, nil
}

// Links are the PCIe links between the card at bdf and its root port, nearest
// the root first. On a virtual machine (virtual is true) only the card's own
// link is read, against its own maximums: a hypervisor's virtual ports
// report whatever they like.
func (r Reader) Links(bdf string, virtual bool) ([]Link, error) {
	chain, err := r.Chain(bdf)
	if err != nil {
		return nil, err
	}
	if virtual && len(chain) > 0 {
		chain = chain[len(chain)-1:]
	}
	var links []Link
	for i, dev := range chain {
		last := i == len(chain)-1
		if !last {
			switch r.PortType(dev) {
			case PortEndpoint, PortLegacyEndpoint, PortUpstream, PortPCIeToPCI:
			default:
				// Root and downstream ports describe the link below them,
				// and a port whose type cannot be read is not guessed at.
				continue
			}
		}
		width, speed, maxWidth, maxSpeed, ok := r.linkState(dev)
		if !ok {
			continue
		}
		link := Link{BDF: dev, Width: width, Speed: speed, ExpectedWidth: maxWidth, ExpectedSpeed: maxSpeed}
		if i > 0 {
			link.Parent = chain[i-1]
			if _, _, pw, ps, pok := r.linkState(link.Parent); pok {
				link.ExpectedWidth = min(link.ExpectedWidth, pw)
				link.ExpectedSpeed = min(link.ExpectedSpeed, ps)
			}
		}
		links = append(links, link)
	}
	return links, nil
}

func (r Reader) linkState(dev string) (width int, speed float64, maxWidth int, maxSpeed float64, ok bool) {
	read := func(name string) string {
		b, err := os.ReadFile(r.path(filepath.Join("sys/bus/pci/devices", dev, name)))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	w, errW := strconv.Atoi(read("current_link_width"))
	mw, errMW := strconv.Atoi(read("max_link_width"))
	s, okS := parseSpeed(read("current_link_speed"))
	ms, okMS := parseSpeed(read("max_link_speed"))
	if errW != nil || errMW != nil || !okS || !okMS || mw <= 0 || ms <= 0 {
		return 0, 0, 0, 0, false
	}
	return w, s, mw, ms, true
}

// parseSpeed reads "16.0 GT/s PCIe" (or "Unknown", which is not a speed).
func parseSpeed(text string) (float64, bool) {
	field, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	v, err := strconv.ParseFloat(field, 64)
	return v, err == nil
}

// PortType reads the device/port type from the PCI Express capability in the
// device's config space, or PortUnknown when it cannot be read (only root may
// read past the first 64 bytes).
func (r Reader) PortType(dev string) int {
	cfg, err := os.ReadFile(r.path(filepath.Join("sys/bus/pci/devices", dev, "config")))
	if err != nil || len(cfg) < 0x40 || cfg[0x06]&0x10 == 0 {
		return PortUnknown
	}
	ptr := int(cfg[0x34]) &^ 0x3
	for seen := 0; ptr >= 0x40 && ptr+3 < len(cfg) && seen < 48; seen++ {
		if cfg[ptr] == 0x10 {
			return int(cfg[ptr+2]>>4) & 0xf
		}
		ptr = int(cfg[ptr+1]) &^ 0x3
	}
	return PortUnknown
}

// AER reads a device's error counters; ok is false where the kernel exposes
// none (no AER, or firmware-first error handling — which reads as zero
// counts, so zero is never proof of health).
func (r Reader) AER(dev string) (AER, bool) {
	total := func(name, key string) (int64, bool) {
		b, err := os.ReadFile(r.path(filepath.Join("sys/bus/pci/devices", dev, name)))
		if err != nil {
			return 0, false
		}
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == key {
				n, err := strconv.ParseInt(fields[1], 10, 64)
				return n, err == nil
			}
		}
		return 0, false
	}
	var a AER
	var okC, okN, okF bool
	a.Correctable, okC = total("aer_dev_correctable", "TOTAL_ERR_COR")
	a.NonFatal, okN = total("aer_dev_nonfatal", "TOTAL_ERR_NONFATAL")
	a.Fatal, okF = total("aer_dev_fatal", "TOTAL_ERR_FATAL")
	return a, okC || okN || okF
}

// Virtual reports whether this host is a virtual machine (the CPU says it
// runs under a hypervisor).
func (r Reader) Virtual() bool {
	b, err := os.ReadFile(r.path("proc/cpuinfo"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "flags") && strings.Contains(" "+line+" ", " hypervisor ") {
			return true
		}
	}
	return false
}
