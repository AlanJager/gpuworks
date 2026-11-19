package extpkg_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// A package that requires the clock, own-state and key calls installs on
// this server, which offers them; a server that does not offer one refuses
// the package as incompatible.
func TestVerifyAcceptsAPackageRequiringTheHostCalls(t *testing.T) {
	w := newWorld(t)
	spec := extsign.Minimal("acme", "acme_backup", "1.0.0")
	spec.Manifest.Permissions = []string{"crypto:derive_key"}
	spec.Manifest.Requires = map[string]int{extapi.CapHostClock: 1, extapi.CapHostStateRead: 1, extapi.CapHostDeriveKey: 1}
	v := w.verifier()
	v.Offered = extpkg.Capabilities()
	if _, err := v.Verify(contents(t, extsign.Build(t, w.pub, spec)), extpkg.Install); err != nil {
		t.Fatal(err)
	}
	older := extpkg.Capabilities()
	delete(older, extapi.CapHostDeriveKey)
	v.Offered = older
	_, err := v.Verify(contents(t, extsign.Build(t, w.pub, spec)), extpkg.Install)
	pe, ok := err.(*messages.ProductError)
	if !ok || pe.Code() != messages.ErrorPluginIncompatible || pe.Params()["capability"] != extapi.CapHostDeriveKey {
		t.Fatalf("on a server without host.derive_key = %v", err)
	}
}
