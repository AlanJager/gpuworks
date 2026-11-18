package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

func nvidiaBindingHost(t *testing.T, drivers []string) string {
	t.Helper()
	root := withHostRoot(t)
	t.Setenv("PATH", t.TempDir()) // Neither host nor container has nvidia-smi.
	for i, driver := range drivers {
		for _, function := range []struct{ suffix, device, class, driver string }{
			{"0", "0x2204", "0x030000", driver},
			{"1", "0x228b", "0x040300", "snd_hda_intel"},
		} {
			bdf := fmt.Sprintf("0000:%02x:00.%s", 0x20+i, function.suffix)
			device := filepath.Join(root, "sys/bus/pci/devices", bdf)
			if err := os.MkdirAll(device, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"vendor": "0x10de", "device": function.device, "class": function.class, "numa_node": "0"} {
				if err := os.WriteFile(filepath.Join(device, name), []byte(value+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if function.driver != "" {
				target := filepath.Join(root, "sys/bus/pci/drivers", function.driver)
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(device, "driver")); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return root
}

// Eight GA102 cards bound to the distribution's nouveau driver, with no
// vendor CLI, reproduce the fresh-node inventory through the public probe.
func TestNVIDIAProbeReportsNouveauAsUnavailableVendorDriver(t *testing.T) {
	root := nvidiaBindingHost(t, []string{"nouveau", "nouveau", "nouveau", "nouveau", "nouveau", "nouveau", "nouveau", "nouveau"})
	var prober Prober = &SMIProber{hostRoot: root}
	cards, err := prober.Probe(t.Context())
	if err != nil || len(cards) != 8 {
		t.Fatalf("Probe = %+v, %v; want eight GPUs without their audio functions", cards, err)
	}
	for _, card := range cards {
		if card.Ownership != accelerator.CardWithoutDriver || accelerator.CardUsable(card.Ownership) {
			t.Errorf("%s ownership=%q; want unavailable vendor driver without external ownership", card.PCIeBDF, card.Ownership)
		}
		if card.Index != -1 || card.UUID != "" || card.MemoryTotalBytes != 0 || card.HealthCode != "" {
			t.Errorf("PCI-only probe invented driver, capacity or fault facts: %+v", card)
		}
	}
}

func TestNVIDIAProbeKeepsOtherDriverOwnershipAndVendorFailuresDistinct(t *testing.T) {
	for _, tc := range []struct{ driver, ownership, health string }{
		{"", accelerator.CardWithoutDriver, ""},
		{"vfio-pci", accelerator.CardPassedThrough, ""},
		{"pci-stub", accelerator.CardHeldElsewhere, ""},
		{"xen-pciback", accelerator.CardHeldElsewhere, ""},
		{"uio_pci_generic", accelerator.CardHeldElsewhere, ""},
		{"nvidia", accelerator.CardOwnedByNode, accelerator.HealthCLIUnanswered},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			var prober Prober = &SMIProber{hostRoot: nvidiaBindingHost(t, []string{tc.driver})}
			cards, err := prober.Probe(t.Context())
			if err != nil || len(cards) != 1 {
				t.Fatalf("Probe = %+v, %v", cards, err)
			}
			card := cards[0]
			if card.Ownership != tc.ownership || card.HealthCode != tc.health || card.MemoryTotalBytes != 0 || card.Index != -1 || card.UUID != "" {
				t.Fatalf("Probe = %+v; want ownership %q / health %q without invented driver facts", card, tc.ownership, tc.health)
			}
		})
	}
}
