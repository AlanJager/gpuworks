package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A node manages one vendor's cards. On a host with cards of more than one
// the operator chooses which (Node.AcceleratorPreference), from the vendors
// the agent reports on the host's bus (Node.AcceleratorVendors).

// NormalizeAcceleratorPreference is the stored spelling of a choice, and
// false when it is not one: auto, or a vendor whose cards a PCI bus lists.
func NormalizeAcceleratorPreference(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == proto.AcceleratorPreferenceAuto || accelerator.HasPCICards(value) {
		return value, true
	}
	return "", false
}

// AcceleratorVendorList is the vendors the agent last reported on the
// host's bus; empty for an agent that reports none, or a column that does
// not decode.
func (n *Node) AcceleratorVendorList() []proto.AcceleratorPresence {
	if n == nil || strings.TrimSpace(n.AcceleratorVendors) == "" {
		return nil
	}
	var vendors []proto.AcceleratorPresence
	if json.Unmarshal([]byte(n.AcceleratorVendors), &vendors) != nil {
		return nil
	}
	return vendors
}

// HostHasAcceleratorVendor reports whether the host's bus has the vendor's
// cards, which is what makes it a vendor the node may be told to manage.
func (n *Node) HostHasAcceleratorVendor(vendor string) bool {
	for _, v := range n.AcceleratorVendorList() {
		if v.Vendor == vendor && v.Cards > 0 {
			return true
		}
	}
	return false
}

// encodeAcceleratorVendors is the column value for a report: empty when it
// lists none, so a node without the choice stores nothing.
func encodeAcceleratorVendors(vendors []proto.AcceleratorPresence) string {
	if len(vendors) == 0 {
		return ""
	}
	b, err := json.Marshal(vendors)
	if err != nil {
		return ""
	}
	return string(b)
}

// SetNodeAcceleratorPreference stores the operator's choice of which
// vendor's cards the node manages. The caller has had the agent accept it.
func (s *Store) SetNodeAcceleratorPreference(uuid, preference string) error {
	stored, ok := NormalizeAcceleratorPreference(preference)
	if !ok {
		return fmt.Errorf("invalid accelerator preference %q", preference)
	}
	result := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("accelerator_preference", stored)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "accelerator_preference")
	return nil
}
