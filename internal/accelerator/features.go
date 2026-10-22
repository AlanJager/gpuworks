package accelerator

// Feature is a platform capability a deployment can ask for beyond running its
// engine on the node's accelerator.
type Feature string

const (
	// FeatureEngineSleep: the engine releases the accelerator's memory on
	// request and takes it back on wake (vLLM sleep mode).
	FeatureEngineSleep Feature = "engine_sleep"
	// FeatureMultiNode: ranks on several nodes joined by a collective
	// transport (NCCL, HCCL).
	FeatureMultiNode Feature = "multi_node"
	// FeatureKVDiskTier: a KV cache tier on the node's disk, in a directory
	// the agent creates and keeps within a quota.
	FeatureKVDiskTier Feature = "kv_disk_tier"
)

// Features lists every Feature, for callers that must name each one.
var Features = []Feature{FeatureEngineSleep, FeatureMultiNode, FeatureKVDiskTier}

// Supports reports whether nodes of vendor provide f. It is the one place this
// is decided; the API, the scheduler and the launch all ask it.
//
// Apple silicon nodes provide none of these. Their engines run as macOS host
// processes: vllm-metal's sleep endpoint answers but frees nothing (measured
// on a 64 GiB MacBook Pro), the engines have no collective transport between Macs, and
// vLLM's disk tier populates its region with madvise(MADV_POPULATE_WRITE),
// which only Linux has (a Darwin kernel number would pass the tier's minimum
// Linux kernel check, so the vendor has to say it). Engine
// parameters such as a host-memory KV pool are not features in this sense:
// the engine decides those (it accepts or refuses the flag).
func Supports(vendor string, f Feature) bool {
	return NormalizeVendor(vendor) != VendorApple
}

// SharedByMemory reports whether a node of vendor shares its accelerator among
// engines by memory rather than handing whole cards out: an Apple silicon
// Mac's one GPU runs several engines at once, each taking its share of the
// same unified memory, so admission counts memory, not free cards.
func SharedByMemory(vendor string) bool {
	return NormalizeVendor(vendor) == VendorApple
}

// Unsupported returns the features of want that nodes of vendor lack, in the
// order given.
func Unsupported(vendor string, want ...Feature) []Feature {
	var missing []Feature
	for _, f := range want {
		if !Supports(vendor, f) {
			missing = append(missing, f)
		}
	}
	return missing
}
