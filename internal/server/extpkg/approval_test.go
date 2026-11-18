package extpkg_test

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// An install that requires approval refuses without the package's own
// approval hash, names the hash and what it covers, and leaves nothing in
// place; with the hash it installs.
func TestInstallRequiresTheApprovalHash(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	raw := pkg(t, w, "1.0.0", func(s *extsign.Spec) {
		s.Manifest.Permissions = []string{"push:banners"}
		s.Manifest.Uses = map[string]int{"table.banners": 1}
	})
	for _, approve := range []extapi.Digest{"", extapi.DigestOf([]byte("something else"))} {
		_, err := h.Install(bytes.NewReader(raw), extpkg.InstallOptions{RequireApproval: true, Approve: approve})
		var pe *messages.ProductError
		if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginApprovalRequired {
			t.Fatalf("approve %q: err = %v", approve, err)
		}
		p := pe.Params()
		if p["id"] != "acme_backup" || p["version"] != "1.0.0" || p["permissions"] != "push:banners" || !extapi.Digest(p["approve_hash"].(string)).Valid() {
			t.Fatalf("params = %v", p)
		}
		if _, err := os.Stat(h.Dir.Package("acme_backup")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused install left %v", err)
		}
	}
	_, err := h.Install(bytes.NewReader(raw), extpkg.InstallOptions{RequireApproval: true})
	var pe *messages.ProductError
	errors.As(err, &pe)
	hash := extapi.Digest(pe.Params()["approve_hash"].(string))
	in, err := h.Install(bytes.NewReader(raw), extpkg.InstallOptions{RequireApproval: true, Approve: hash})
	if err != nil || in.ApprovalHash != hash {
		t.Fatalf("approved install = %+v, %v", in, err)
	}
	ctl, _, err := h.Dir.ReadControl("acme_backup")
	if err != nil || ctl.Enabled || ctl.ApprovedHash != string(hash) {
		t.Fatalf("control = %+v, %v", ctl, err)
	}
}

// SetEnabled flips only the enabled flag, and refuses a package that is
// not installed.
func TestSetEnabled(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	if err := h.SetEnabled("acme_backup", true); !isCode(err, messages.ErrorPluginNotInstalled) {
		t.Fatalf("enable before install = %v", err)
	}
	if err := h.SetEnabled("../etc", true); !isCode(err, messages.ErrorPluginNotInstalled) {
		t.Fatalf("enable of a bad id = %v", err)
	}
	in, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{ApprovedBy: "root"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{true, false, true} {
		if err := h.SetEnabled("acme_backup", want); err != nil {
			t.Fatal(err)
		}
		ctl, _, err := h.Dir.ReadControl("acme_backup")
		if err != nil || ctl.Enabled != want || ctl.ApprovedHash != string(in.ApprovalHash) || ctl.ApprovedBy != "root" {
			t.Fatalf("control = %+v, %v; want enabled %v", ctl, err, want)
		}
	}
	if err := h.Remove("acme_backup", false); err != nil {
		t.Fatal(err)
	}
	if err := h.SetEnabled("acme_backup", true); !isCode(err, messages.ErrorPluginNotInstalled) {
		t.Fatalf("enable after remove = %v", err)
	}
}

// The server offers every pushed table kind at its newest schema, plus
// node/host identity reads, clock/state/key calls and host setup at version 1.
func TestCapabilitiesOfferRoutesAndThePushedTables(t *testing.T) {
	caps, offer := extpkg.Capabilities(), extpkg.Offer()
	for _, kind := range extapi.TableKinds() {
		name := extapi.TableCapability(kind)
		schemas := extapi.TableSchemas(kind)
		if caps[name] != schemas[len(schemas)-1] || offer[name] != (extapi.Range{Min: 1, Max: caps[name]}) {
			t.Fatalf("%s: capabilities %d, offer %+v", name, caps[name], offer[name])
		}
	}
	for _, name := range []string{extapi.CapHostNodeIdentity, extapi.CapHostNodes, extapi.CapHostClock, extapi.CapHostStateRead, extapi.CapHostDeriveKey, extapi.CapHostSetup, extapi.CapHostHostIdentity} {
		if caps[name] != 1 || offer[name] != (extapi.Range{Min: 1, Max: 1}) {
			t.Fatalf("%s: capabilities %d, offer %+v", name, caps[name], offer[name])
		}
	}
	if caps[extapi.CapRoutes] != 1 || offer[extapi.CapRoutes] != (extapi.Range{Min: 1, Max: 1}) || caps[extapi.CapRoutesWebSocket] != 0 {
		t.Fatalf("routes: capabilities %v", caps)
	}
	if len(caps) != len(extapi.TableKinds())+8 || len(offer) != len(caps) {
		t.Fatalf("capabilities = %v", caps)
	}
}

func isCode(err error, code string) bool {
	var pe *messages.ProductError
	return errors.As(err, &pe) && pe.Code() == code
}
