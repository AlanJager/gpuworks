// Package hostsetup renders a package's host setup steps (the closed
// host_setup vocabulary of the manifest) into what root applies on the
// server host, and into the sandbox the plugin then runs with:
//
//   - one udev rule file per package, which gives the device group
//     (Group) read and write access to the USB devices the package
//     declares, and to nothing else;
//   - a root-owned applied record per package, holding the digest of the
//     steps root applied, which the server compares before it grants the
//     plugin any device access;
//   - the Landlock device directories and read paths (Landlock).
//
// The package imports nothing from this repository but pkg/extapi, so the
// command line, the install hook and the plugin host share one rendering.
// See docs/dev/extensions.md ("主机配置").
package hostsetup

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Group is the system group the rule files give device access to; the
// service account is added to it.
const Group = "gpuworks-usb"

// Default locations of what host setup writes.
const (
	RulesDir   = "/etc/udev/rules.d"
	AppliedDir = "/etc/gpuworks/plugins"
)

const (
	rulePrefix    = "70-gpuworks-plugin-"
	ruleSuffix    = ".rules"
	appliedSuffix = ".applied"
	deviceMode    = "0660"
)

// RuleFile is the name of package id's rule file in RulesDir. The id is
// validated first, so nothing but a package id reaches a file name.
func RuleFile(id string) (string, error) {
	if !extapi.ValidPackageID(id) {
		return "", fmt.Errorf("hostsetup: %q is not a package id", id)
	}
	return rulePrefix + id + ruleSuffix, nil
}

// Command is what an administrator runs as root to apply package id's
// host setup, as PLUGIN_HOST_SETUP_REQUIRED names it.
func Command(id string) string { return "sudo gpuworks plugin host-setup --id " + id }

// PruneCommand removes the rule files and records of packages that are no
// longer installed.
const PruneCommand = "sudo gpuworks plugin host-setup --prune"

// ruleFileID is the package id a rule file name belongs to, or "" when
// the name is not one RuleFile makes.
func ruleFileID(name string) string {
	id, ok := strings.CutPrefix(name, rulePrefix)
	if !ok {
		return ""
	}
	if id, ok = strings.CutSuffix(id, ruleSuffix); !ok || !extapi.ValidPackageID(id) {
		return ""
	}
	return id
}

// validSteps refuses steps outside the vocabulary this package renders;
// the manifest schema already refuses them, so this guards the renderer
// against a caller that skipped verification.
func validSteps(steps []extapi.HostSetupStep) error {
	if len(steps) == 0 {
		return errors.New("hostsetup: no host setup steps")
	}
	for _, s := range steps {
		if s.Type != extapi.HostSetupUSBDevice || !usbID(s.VendorID) || !usbID(s.ProductID) {
			return fmt.Errorf("hostsetup: step %+v is not a usb_device step with four lowercase hex digit ids", s)
		}
	}
	return nil
}

func usbID(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sortedSteps is steps in the order they are rendered and digested: by
// type, vendor and product, so the order a manifest lists them in changes
// neither the file nor the applied digest.
func sortedSteps(steps []extapi.HostSetupStep) []extapi.HostSetupStep {
	out := slices.Clone(steps)
	slices.SortFunc(out, func(a, b extapi.HostSetupStep) int {
		return strings.Compare(a.Type+a.VendorID+a.ProductID, b.Type+b.VendorID+b.ProductID)
	})
	return slices.Compact(out)
}

// Rules is the udev rule file of package id. Each usb_device step matches
// the device by vendor and product and sets the group and mode on its two
// device nodes only: the libusb node under /dev/bus/usb and any hidraw
// node of the device. It adds no symlink: the sandbox checks the inode a
// path resolves to, so a link would grant nothing and only clutter /dev.
func Rules(id string, steps []extapi.HostSetupStep) ([]byte, error) {
	if _, err := RuleFile(id); err != nil {
		return nil, err
	}
	if err := validSteps(steps); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Written by gpuworks plugin host-setup for plugin %s; do not edit.\n", id)
	fmt.Fprintf(&b, "# Group %s may open these USB devices; the plugin's sandbox grants the device directories.\n", Group)
	for _, s := range sortedSteps(steps) {
		fmt.Fprintf(&b, "SUBSYSTEM==\"usb\", ENV{DEVTYPE}==\"usb_device\", ATTR{idVendor}==\"%s\", ATTR{idProduct}==\"%s\", GROUP=\"%s\", MODE=\"%s\"\n",
			s.VendorID, s.ProductID, Group, deviceMode)
		fmt.Fprintf(&b, "SUBSYSTEM==\"hidraw\", ATTRS{idVendor}==\"%s\", ATTRS{idProduct}==\"%s\", GROUP=\"%s\", MODE=\"%s\"\n",
			s.VendorID, s.ProductID, Group, deviceMode)
	}
	return b.Bytes(), nil
}

// Digest is what an applied record holds for steps: the digest of their
// canonical form, in rendering order. An upgrade that keeps the steps keeps
// the digest, so it needs no new root step.
func Digest(steps []extapi.HostSetupStep) (extapi.Digest, error) {
	if err := validSteps(steps); err != nil {
		return "", err
	}
	list := []any{}
	for _, s := range sortedSteps(steps) {
		list = append(list, map[string]any{"type": s.Type, "vendor_id": s.VendorID, "product_id": s.ProductID})
	}
	return extapi.DigestValue(map[string]any{"host_setup": list})
}
