package accelerator

import (
	"regexp"
	"sort"
	"strings"
)

// KubeProfile is what running a vendor's cards on a Kubernetes node takes:
// the host paths the agent pod mounts so its probe finds the vendor's CLI
// and driver libraries where the host has them, the environment those need,
// and whether the agent itself runs under the vendor's RuntimeClass.
type KubeProfile struct {
	Vendor string
	// RuntimeClass: the agent pod runs under the cluster's RuntimeClass,
	// which injects the CLI and devices (NVIDIA's runtime does). A cluster
	// without one leaves it to the node's default runtime, which injects
	// them on managed clusters; elsewhere the agent runs CLI on the host.
	RuntimeClass bool
	// CLI is the vendor's probe CLI that RuntimeClass injects.
	CLI string
	// HostDirs and HostFiles are mounted read-only at the same path.
	HostDirs  []string
	HostFiles []string
	// Env is set on the agent container.
	Env map[string]string
	// HostCLIs: the agent runs the vendor's CLIs in the host's mount
	// namespace, with the host's C library and driver libraries, which a
	// container of another distribution cannot load. The mounts then only
	// serve the files the probe reads.
	HostCLIs bool
}

var kubeProfiles = map[string]KubeProfile{
	VendorNVIDIA: {Vendor: VendorNVIDIA, RuntimeClass: true, CLI: "nvidia-smi",
		Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "all", "NVIDIA_DRIVER_CAPABILITIES": "compute,utility"}},
	// The probes read the driver's version files under /usr/local; the CLIs
	// themselves run on the host.
	VendorAscend: {Vendor: VendorAscend, HostCLIs: true,
		HostDirs:  []string{"/usr/local"},
		HostFiles: []string{"/etc/ascend_install.info"}},
	VendorHygon: {Vendor: VendorHygon, HostCLIs: true,
		HostDirs: []string{"/opt/hyhal", "/opt/dtk"}},
	VendorPPU: {Vendor: VendorPPU, HostCLIs: true,
		HostDirs: []string{"/usr/local"}},
}

// KubeProfileFor returns the profile of a vendor that can run on Kubernetes.
func KubeProfileFor(vendor string) (KubeProfile, bool) {
	p, ok := kubeProfiles[NormalizeVendor(vendor)]
	return p, ok
}

// KubeVendors lists the vendors with a Kubernetes profile.
func KubeVendors() []string {
	out := make([]string, 0, len(kubeProfiles))
	for v := range kubeProfiles {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// KubeResourceKind says what a Kubernetes extended resource is to the
// platform.
type KubeResourceKind int

const (
	// KubeResourceOther is not an accelerator resource.
	KubeResourceOther KubeResourceKind = iota
	// KubeResourceCard is one whole card per unit.
	KubeResourceCard
	// KubeResourceShared hands out parts of cards (time-slicing, MIG, HAMi,
	// vNPU): a unit is not a card the platform could own.
	KubeResourceShared
)

var ascendSlice = regexp.MustCompile(`^Ascend[0-9A-Za-z]*-\d+c`)

// ClassifyKubeResource classifies an extended resource a device plugin
// advertises. The vendors' own plugins name whole cards nvidia.com/gpu,
// huawei.com/Ascend910 (or Ascend310P, npu…), hygon.com/dcu, amd.com/gpu and
// alibabacloud.com/ppu; the names below them are partitions or shares of a
// card, and so is any ".shared", ".sliced" or ".partitioned" variant.
func ClassifyKubeResource(resource string) (KubeResourceKind, string) {
	domain, name, ok := strings.Cut(resource, "/")
	if !ok {
		return KubeResourceOther, ""
	}
	vendor := ""
	switch domain {
	case "nvidia.com":
		vendor = VendorNVIDIA
	case "huawei.com":
		vendor = VendorAscend
	case "hygon.com":
		vendor = VendorHygon
	case "amd.com":
		vendor = VendorAMD
	case "alibabacloud.com":
		vendor = VendorPPU
	default:
		return KubeResourceOther, ""
	}
	for _, part := range []string{".shared", ".sliced", ".partitioned"} {
		if strings.Contains(name, part) {
			return KubeResourceShared, vendor
		}
	}
	switch {
	case vendor == VendorNVIDIA && name == "gpu",
		vendor == VendorAMD && name == "gpu",
		vendor == VendorHygon && name == "dcu",
		vendor == VendorPPU && name == "ppu",
		vendor == VendorAscend && (name == "npu" || (strings.HasPrefix(name, "Ascend") && !ascendSlice.MatchString(name))):
		return KubeResourceCard, vendor
	case vendor == VendorNVIDIA && (strings.HasPrefix(name, "mig-") || strings.HasPrefix(name, "gpumem") || strings.HasPrefix(name, "gpucores")),
		vendor == VendorHygon && strings.HasPrefix(name, "dcu"),
		vendor == VendorAscend && ascendSlice.MatchString(name):
		return KubeResourceShared, vendor
	}
	return KubeResourceOther, ""
}
