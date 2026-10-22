package nodelabel

import (
	"fmt"
	"sort"
	"strconv"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// Requirements is a deployment's node requirements: for each key, the values
// a node may carry. Values of one key are alternatives ("any of"); different
// keys must all hold. A key with a single value is plain equality.
type Requirements map[string][]string

// MaxValuesPerKey bounds the alternatives of one key. A manual node list is
// the longest a requirement gets, and this holds one for a large cluster.
const MaxValuesPerKey = 256

// NormalizeRequirements validates a requirement set and returns a copy with
// each key's values deduplicated and sorted, so two spellings of the same
// requirement are stored identically. The input is not modified.
//
// A key either names a written label - any key a node's labels may carry -
// or one of the system label keys. A key inside the reserved domain that is
// not a system key is refused rather than accepted as a requirement that
// matches nothing: it is a misspelling, and an empty candidate pool is a
// worse way to find that out.
//
// Rules a single requirement cannot judge on its own - that a deployment
// names exactly one vendor, that a named node exists - belong to the caller.
func NormalizeRequirements(in Requirements) (Requirements, error) {
	if len(in) > MaxKeys {
		return nil, fmt.Errorf("node requirements must name at most %d keys", MaxKeys)
	}
	out := make(Requirements, len(in))
	for key, values := range in {
		if err := validKey(key); err != nil {
			return nil, err
		}
		system := IsSystemKey(key)
		if !system && Reserved(key) {
			return nil, fmt.Errorf("node requirement key %q is not a system label in the reserved %s namespace", key, Domain)
		}
		normalized, err := requirementValues(key, values, system)
		if err != nil {
			return nil, err
		}
		out[key] = normalized
	}
	return out, nil
}

func requirementValues(key string, values []string, system bool) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("node requirement %q names no value", key)
	}
	if len(values) > MaxValuesPerKey {
		return nil, fmt.Errorf("node requirement %q names more than %d values", key, MaxValuesPerKey)
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if err := validValue(key, value); err != nil {
			return nil, err
		}
		if system {
			if err := validSystemValue(key, value); err != nil {
				return nil, err
			}
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	if key == KeyVendor && len(out) != 1 {
		// One deployment carries one image, which runs on one vendor.
		return nil, fmt.Errorf("node requirement %q must name exactly one vendor", key)
	}
	return out, nil
}

// validSystemValue applies what is known about a system key's values. An
// empty value never matches: no system label is generated without one. The
// keys not listed take their value from free text (a GPU model) or from a
// vocabulary that grows with the hardware (an architecture), so a value no
// node carries today is a requirement that matches nothing today, which the
// candidate count shows - not an error.
func validSystemValue(key, value string) error {
	if value == "" {
		return fmt.Errorf("node requirement %q names an empty value", key)
	}
	switch key {
	case KeyVendor:
		if !knownVendor(value) {
			return fmt.Errorf("node requirement %q names unknown vendor %q", key, value)
		}
	case KeyComputeNetwork, KeyStorageSyncNetwork, KeyServingNetwork:
		if value == ManagementNetwork {
			return nil
		}
		if id, err := strconv.ParseUint(value, 10, 64); err != nil || id == 0 || strconv.FormatUint(id, 10) != value {
			return fmt.Errorf("node requirement %q names %q, which is neither a network id nor %q", key, value, ManagementNetwork)
		}
	}
	return nil
}

func knownVendor(value string) bool {
	for _, vendor := range accelerator.KnownVendors() {
		if value == vendor {
			return true
		}
	}
	return false
}
