package extpkg_test

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// A package with a usb_device host step verifies on Linux now that the
// server offers host.setup@1, is refused as incompatible by a server that
// does not offer it, and is refused on macOS before anything is installed.
func TestVerifyAPackageWithHostSetup(t *testing.T) {
	w := newWorld(t)
	spec := extsign.Minimal("acme", "acme_key", "1.0.0")
	spec.Manifest.Requires = map[string]int{extapi.CapHostSetup: 1}
	spec.Manifest.HostSetup = []extapi.HostSetupStep{{Type: extapi.HostSetupUSBDevice, VendorID: "096e", ProductID: "0201"}}
	b := extsign.Build(t, w.pub, spec)

	v := w.verifier()
	v.Offered = extpkg.Capabilities()
	got, err := v.Verify(contents(t, b), extpkg.Install)
	if err != nil {
		t.Fatalf("a package with host setup on linux: %v", err)
	}
	if len(got.Manifest.HostSetup) != 1 {
		t.Fatalf("verified host setup = %+v", got.Manifest.HostSetup)
	}

	if _, err := w.verifier().Verify(contents(t, b), extpkg.Install); !isCode(err, messages.ErrorPluginIncompatible) {
		t.Fatalf("without host.setup offered = %v", err)
	}

	mac := w.verifier()
	mac.Offered, mac.Platform = extpkg.Capabilities(), extpkg.Platform{OS: "darwin", Arch: "arm64"}
	_, err = mac.Verify(contents(t, b), extpkg.Install)
	if !isCode(err, messages.ErrorPluginHostSetupUnsupported) {
		t.Fatalf("on darwin = %v", err)
	}
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Params()["id"] != "acme_key" {
		t.Fatalf("on darwin = %v", err)
	}
	plain := extsign.Minimal("acme", "acme_plain", "1.0.0")
	if _, err := mac.Verify(contents(t, extsign.Build(t, w.pub, plain)), extpkg.Install); err != nil {
		t.Fatalf("a package without host setup on darwin: %v", err)
	}
}
