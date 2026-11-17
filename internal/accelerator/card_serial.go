package accelerator

import "strings"

// CardSerial is the card's own hardware identity as its probe reported it
// (GPUInfo.UUID), and whether it is one: a value the card carries and keeps
// across slots, hosts, reboots and driver reinstalls, so it can name the
// physical card. It is false where the probe had no such value and made
// the UUID up from where the card sits:
//
//   - NVIDIA, PPU, Kunlunxin: the vendor CLI's card UUID — hardware.
//   - AMD: "amd-<unique_id>", the ASIC serial — hardware; "amd-<PCI
//     address>" when the driver exposes no unique_id — a slot.
//   - Hygon: "GPU-<unique id>" from hy-smi or sysfs — hardware;
//     "hygon-<PCI address>" or "hygon-<index>" without one — a slot.
//   - Ascend: "ascend-<index>" — an index; npu-smi's table shows no serial.
//   - Apple: "APPLE-<IOPlatformUUID>" — the machine's, the GPU being part of
//     the SoC; stable, and the same machine as the host's system UUID.
//   - The fake probers' "GPU-FAKE-<n>" and "FAKE-<vendor>-<n>" — test values
//     that repeat on every node.
func CardSerial(uuid string) (string, bool) {
	uuid = strings.TrimSpace(uuid)
	lower := strings.ToLower(uuid)
	switch {
	case uuid == "",
		strings.HasPrefix(lower, "ascend-"),
		strings.HasPrefix(lower, "hygon-"),
		strings.HasPrefix(lower, "gpu-fake-"),
		strings.HasPrefix(lower, "fake-"),
		strings.HasPrefix(lower, "amd-") && strings.Contains(lower, ":"):
		return "", false
	}
	return uuid, true
}
