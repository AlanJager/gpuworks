package pcilink

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree lays out a host's sysfs from a capture: one line per device, nearest
// the root first — address, current speed, current width, max speed, max
// width, then the first 256 bytes of its config space in hex. The devices
// nest under one root complex the way /sys/devices does, and
// /sys/bus/pci/devices links to each.
func tree(t *testing.T, capture string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", capture))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "sys/devices/pci0000:00")
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, "\t")
		bdf := f[0]
		dir = filepath.Join(dir, bdf)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{
			"current_link_speed": f[1], "current_link_width": f[2],
			"max_link_speed": f[3], "max_link_width": f[4],
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(f[5]), " ", ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config"), cfg, 0o644); err != nil {
			t.Fatal(err)
		}
		bus := filepath.Join(root, "sys/bus/pci/devices")
		if err := os.MkdirAll(bus, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, filepath.Join(bus, bdf)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A K100 behind a PCIe switch (172.25.17.133, captured idle on 2026-11-01):
// root port 64 → switch upstream port 65 → downstream port 66 → card 67.
func TestLinksBehindASwitch(t *testing.T) {
	r := Reader{Root: tree(t, "hygon-k100-switch.tsv")}
	types := map[string]int{"0000:64:00.0": PortRoot, "0000:65:00.0": PortUpstream, "0000:66:00.0": PortDownstream, "0000:67:00.0": PortLegacyEndpoint} // the K100 says so itself
	for dev, want := range types {
		if got := r.PortType(dev); got != want {
			t.Errorf("PortType(%s) = %d, want %d", dev, got, want)
		}
	}
	links, err := r.Links("0000:67:00.0", false)
	if err != nil {
		t.Fatal(err)
	}
	// Two physical links: the switch's uplink (as its upstream port sees
	// it) and the card's own. The root and downstream ports describe the
	// same links from the other end and are not counted again.
	if len(links) != 2 || links[0].BDF != "0000:65:00.0" || links[1].BDF != "0000:67:00.0" {
		t.Fatalf("links = %+v", links)
	}
	up := links[0]
	// The uplink is x8 Gen3 at most (the root port's maximum), negotiated
	// x8 — but idle it runs at 2.5 GT/s. That is why speed is only judged on
	// a busy card.
	if up.Parent != "0000:64:00.0" || up.ExpectedWidth != 8 || up.ExpectedSpeed != 8 || up.Narrow() || !up.Slow() {
		t.Fatalf("uplink = %+v", up)
	}
	card := links[1]
	if card.Parent != "0000:66:00.0" || card.Narrow() || card.Slow() || card.Width != 16 || card.Speed != 16 {
		t.Fatalf("card link = %+v", card)
	}
}

// An RTX 8000 that can do x16 in a root port that can only do x8 (gpu3,
// 2026-11-01): x8 is all both ends can do, so the link is not narrow.
func TestTheNarrowerEndSetsTheExpectation(t *testing.T) {
	r := Reader{Root: tree(t, "gpu3-rtx8000-rootport.tsv")}
	links, err := r.Links("0000:17:00.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].ExpectedWidth != 8 || links[0].Narrow() || links[0].Slow() {
		t.Fatalf("links = %+v", links)
	}
	// On a virtual machine only the card's own maximums count: the virtual
	// port above it is not believed. Here that makes x8 of x16 narrow.
	links, err = r.Links("0000:17:00.0", true)
	if err != nil || len(links) != 1 || links[0].Parent != "" || !links[0].Narrow() {
		t.Fatalf("virtual: links = %+v err=%v", links, err)
	}
}

func TestAERAndVirtual(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "sys/bus/pci/devices/0000:67:00.0")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	r := Reader{Root: root}
	if _, ok := r.AER("0000:67:00.0"); ok {
		t.Fatal("a device without AER files reported counters")
	}
	// The kernel's format, as read on 172.25.17.133.
	files := map[string]string{
		"aer_dev_correctable": "RxErr 0\nBadTLP 2\nBadDLLP 1\nTOTAL_ERR_COR 3\n",
		"aer_dev_nonfatal":    "Undefined 0\nDLP 0\nTOTAL_ERR_NONFATAL 0\n",
		"aer_dev_fatal":       "Undefined 0\nDLP 0\nSDES 0\nTOTAL_ERR_FATAL 1\n",
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dev, name), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, ok := r.AER("0000:67:00.0")
	if !ok || a != (AER{Correctable: 3, NonFatal: 0, Fatal: 1}) {
		t.Fatalf("AER = %+v %v", a, ok)
	}
	if r.Virtual() {
		t.Fatal("no cpuinfo read as virtual")
	}
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/cpuinfo"), []byte("processor\t: 0\nflags\t\t: fpu vme hypervisor lahf_lm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !r.Virtual() {
		t.Fatal("a hypervisor flag did not read as virtual")
	}
}

func TestUnreadableConfigIsUnknownNotGuessed(t *testing.T) {
	root := tree(t, "hygon-k100-switch.tsv")
	// A non-root agent reads only the first 64 bytes of config space.
	cfg := filepath.Join(root, "sys/bus/pci/devices/0000:65:00.0/config")
	b, _ := os.ReadFile(cfg)
	if err := os.WriteFile(cfg, b[:64], 0o644); err != nil {
		t.Fatal(err)
	}
	r := Reader{Root: root}
	if got := r.PortType("0000:65:00.0"); got != PortUnknown {
		t.Fatalf("PortType = %d, want unknown", got)
	}
	links, _ := r.Links("0000:67:00.0", false)
	if len(links) != 1 || links[0].BDF != "0000:67:00.0" {
		t.Fatalf("links = %+v, want only the card's own", links)
	}
}
