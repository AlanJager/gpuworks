package extpkg_test

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// v1Asks and v2Asks are two versions that differ in every part an
// approval diff compares.
func v1Asks(s *extsign.Spec) {
	s.Manifest.Permissions = []string{"http:routes", "push:banners"}
	s.Manifest.Requires = map[string]int{"routes": 1}
	s.Manifest.Uses = map[string]int{"table.banners": 1}
	s.Manifest.Routes = []extapi.RouteDecl{routeDecl("/jobs", extapi.RouteAdmin)}
	s.Manifest.Features = []extapi.FeatureDecl{{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf}}
	s.Manifest.Runtime.Devices = []string{"/dev/ttyUSB0"}
}

func v2Asks(s *extsign.Spec) {
	s.Manifest.Permissions = []string{"http:public", "http:routes"}
	s.Manifest.Requires = map[string]int{"host.setup": 1, "routes": 1}
	hook := routeDecl("/hooks", extapi.RoutePublic)
	hook.Method = "POST"
	s.Manifest.Routes = []extapi.RouteDecl{routeDecl("/jobs", extapi.RouteAdmin), hook}
	s.Manifest.Features = []extapi.FeatureDecl{{Key: "acme.tools", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf}}
	s.Manifest.HostSetup = []extapi.HostSetupStep{{Type: extapi.HostSetupUSBDevice, VendorID: "1234", ProductID: "abcd"}}
	s.Manifest.Runtime.Devices = []string{"/dev/ttyUSB0", "/dev/ttyUSB1"}
}

// upgradeHost is a host whose verifier also offers host setup, with the
// site policy's auto-approval set as given.
func upgradeHost(t *testing.T, w *world, autoApprove bool) *extpkg.Host {
	t.Helper()
	h, _ := newHost(t, w)
	h.Verifier.Offered["host.setup"] = 1
	h.Verifier.Policy.AutoApproveUnchanged = autoApprove
	return h
}

func approvalOf(t *testing.T, err error) (map[string]any, *extpkg.ApprovalNeeded) {
	t.Helper()
	var pe *messages.ProductError
	var need *extpkg.ApprovalNeeded
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginApprovalRequired || !errors.As(err, &need) {
		t.Fatalf("err = %v, want %s", err, messages.ErrorPluginApprovalRequired)
	}
	return pe.Params(), need
}

// An upgrade without approval is refused with the exact diff from the
// installed version, in the error's params and in its cause, and nothing
// changes on disk.
func TestUpgradeRefusalCarriesTheExactDiff(t *testing.T) {
	w := newWorld(t)
	h := upgradeHost(t, w, true)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", v1Asks)), extpkg.InstallOptions{Enable: true, ApprovedBy: "root"}); err != nil {
		t.Fatal(err)
	}
	before, _, _ := h.Dir.ReadControl("acme_backup")
	_, err := h.Upgrade(bytes.NewReader(pkg(t, w, "2.0.0", v2Asks)), extpkg.UpgradeOptions{ApprovedBy: "ops"})
	params, need := approvalOf(t, err)
	if !need.Hash.Valid() {
		t.Fatalf("hash = %q", need.Hash)
	}
	wantDiff := extpkg.ApprovalDiff{
		FromVersion: "1.0.0", ToVersion: "2.0.0", HashChanged: true,
		PermissionsAdded:    []extpkg.PermissionChange{{Permission: "http:public", Risk: extpkg.RiskElevated}},
		PermissionsRemoved:  []extpkg.PermissionChange{{Permission: "push:banners", Risk: extpkg.RiskLow}},
		CapabilitiesAdded:   []string{"host.setup@1"},
		CapabilitiesRemoved: []string{"table.banners@1"},
		FeaturesAdded:       []string{"acme.tools"},
		FeaturesRemoved:     []string{"acme.backup"},
		RoutesAdded:         []string{"POST /hooks public"},
		HostSetupAdded:      []string{"usb_device 1234:abcd"},
		DevicesAdded:        []string{"/dev/ttyUSB1"},
		Isolation:           extpkg.Change{From: extapi.IsolationLandlock, To: extapi.IsolationLandlock},
	}
	if !reflect.DeepEqual(need.Diff, wantDiff) {
		t.Fatalf("diff = %+v\nwant %+v", need.Diff, wantDiff)
	}
	wantParams := map[string]any{
		"id": "acme_backup", "version": "2.0.0", "approve_hash": string(need.Hash),
		"permissions": "http:public,http:routes", "roles": "", "from_version": "1.0.0",
		"permissions_added": "http:public=elevated", "permissions_removed": "push:banners=low",
		"roles_added": "", "roles_removed": "",
		"capabilities_added": "host.setup@1", "capabilities_removed": "table.banners@1",
		"features_added": "acme.tools", "features_removed": "acme.backup",
		"routes_added": "POST /hooks public", "routes_removed": "",
		"host_setup_added": "usb_device 1234:abcd", "host_setup_removed": "",
		"devices_added": "/dev/ttyUSB1", "devices_removed": "",
		"ui_mode": "", "isolation": "",
	}
	if !reflect.DeepEqual(params, wantParams) {
		t.Fatalf("params = %v\nwant %v", params, wantParams)
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current = %q after a refused upgrade", v)
	}
	if versions, _ := h.Dir.Versions("acme_backup"); !slices.Equal(versions, []string{"1.0.0"}) {
		t.Fatalf("versions = %v after a refused upgrade", versions)
	}
	if after, _, _ := h.Dir.ReadControl("acme_backup"); after != before {
		t.Fatalf("control = %+v, was %+v", after, before)
	}
	// With the hash the refusal named, the same upgrade applies.
	up, err := h.Upgrade(bytes.NewReader(pkg(t, w, "2.0.0", v2Asks)), extpkg.UpgradeOptions{ApprovedBy: "ops", Approve: need.Hash})
	if err != nil || up.Previous != "1.0.0" || up.AutoApproved || !reflect.DeepEqual(up.Diff, wantDiff) {
		t.Fatalf("approved upgrade = %+v, %v", up, err)
	}
}

// A first install's refusal carries the same keys, as the diff from
// nothing, so the code's params are the same at every call site.
func TestInstallRefusalIsTheDiffFromNothing(t *testing.T) {
	w := newWorld(t)
	h := upgradeHost(t, w, false)
	_, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", v1Asks)), extpkg.InstallOptions{RequireApproval: true})
	params, need := approvalOf(t, err)
	want := map[string]any{
		"id": "acme_backup", "version": "1.0.0", "approve_hash": string(need.Hash),
		"permissions": "http:routes,push:banners", "roles": "", "from_version": "",
		"permissions_added": "http:routes=low,push:banners=low", "permissions_removed": "",
		"roles_added": "", "roles_removed": "",
		"capabilities_added": "routes@1,table.banners@1", "capabilities_removed": "",
		"features_added": "acme.backup", "features_removed": "",
		"routes_added": "GET /jobs admin", "routes_removed": "",
		"host_setup_added": "", "host_setup_removed": "",
		"devices_added": "/dev/ttyUSB0", "devices_removed": "",
		"ui_mode": "", "isolation": "->" + extapi.IsolationLandlock,
	}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("params = %v\nwant %v", params, want)
	}
}

// The approval matrix: an upgrade applies with the right approval, or
// without one only when its hash is the approved one and the site policy
// auto-approves that; every other combination is refused and changes
// nothing.
func TestUpgradeApprovalPolicyMatrix(t *testing.T) {
	sameAsks := func(s *extsign.Spec) {} // the approval hash of 1.0.0
	moreAsks := func(s *extsign.Spec) {
		s.Manifest.Permissions = []string{"push:banners"}
		s.Manifest.Uses = map[string]int{"table.banners": 1}
	}
	for _, auto := range []bool{false, true} {
		for _, changed := range []bool{false, true} {
			for _, approve := range []string{"none", "wrong", "right"} {
				t.Run(fmt.Sprintf("auto=%v changed=%v approve=%s", auto, changed, approve), func(t *testing.T) {
					w := newWorld(t)
					h := upgradeHost(t, w, auto)
					in, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{ApprovedBy: "root"})
					if err != nil {
						t.Fatal(err)
					}
					edit := sameAsks
					if changed {
						edit = moreAsks
					}
					raw := pkg(t, w, "1.1.0", edit)
					// The right hash is the one an install elsewhere names.
					_, refusal := upgradeHost(t, w, false).Install(bytes.NewReader(raw), extpkg.InstallOptions{RequireApproval: true})
					_, need := approvalOf(t, refusal)
					rightHash := need.Hash
					if (rightHash == in.ApprovalHash) == changed {
						t.Fatalf("hash %s vs installed %s with changed=%v", rightHash, in.ApprovalHash, changed)
					}
					opt := extpkg.UpgradeOptions{ApprovedBy: "ops"}
					switch approve {
					case "wrong":
						opt.Approve = extapi.DigestOf([]byte("something else"))
					case "right":
						opt.Approve = rightHash
					}
					up, err := h.Upgrade(bytes.NewReader(raw), opt)
					autoApplies := auto && !changed
					wantOK := approve == "right" || autoApplies
					ctl, _, _ := h.Dir.ReadControl("acme_backup")
					current, _ := h.Dir.Current("acme_backup")
					if !wantOK {
						params, need := approvalOf(t, err)
						if need.Diff.HashChanged != changed || params["from_version"] != "1.0.0" || params["version"] != "1.1.0" {
							t.Fatalf("refusal = %v, diff %+v", params, need.Diff)
						}
						if current != "1.0.0" || ctl.ApprovedHash != string(in.ApprovalHash) || ctl.ApprovedBy != "root" {
							t.Fatalf("a refused upgrade changed current %q, control %+v", current, ctl)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					wantBy := "ops"
					if autoApplies {
						wantBy = "root" // nothing new was approved
					}
					if up.AutoApproved != autoApplies || up.Diff.HashChanged != changed || current != "1.1.0" ||
						ctl.ApprovedHash != string(rightHash) || ctl.ApprovedBy != wantBy {
						t.Fatalf("upgrade = %+v, current %q, control %+v", up, current, ctl)
					}
				})
			}
		}
	}
}

// An upgrade keeps whether the package is enabled, on or off, and keeps
// applied host setup unless the new version asks for another step.
func TestUpgradeKeepsEnabledAndAppliedHostSetup(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		w := newWorld(t)
		h := upgradeHost(t, w, true)
		withSetup := func(s *extsign.Spec) {
			s.Manifest.Requires = map[string]int{"host.setup": 1}
			s.Manifest.HostSetup = []extapi.HostSetupStep{{Type: extapi.HostSetupUSBDevice, VendorID: "1234", ProductID: "abcd"}}
		}
		in, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", withSetup)), extpkg.InstallOptions{Enable: enabled, ApprovedBy: "root"})
		if err != nil {
			t.Fatal(err)
		}
		ctl, _, _ := h.Dir.ReadControl("acme_backup")
		ctl.HostSetupApplied = true
		if err := h.Dir.WriteControl("acme_backup", ctl); err != nil {
			t.Fatal(err)
		}
		// Same asks: auto-approved, setup still applied.
		if _, err := h.Upgrade(bytes.NewReader(pkg(t, w, "1.1.0", withSetup)), extpkg.UpgradeOptions{}); err != nil {
			t.Fatal(err)
		}
		got, _, _ := h.Dir.ReadControl("acme_backup")
		if got.Enabled != enabled || !got.HostSetupApplied || got.ApprovedHash != string(in.ApprovalHash) {
			t.Fatalf("enabled=%v: control after 1.1.0 = %+v", enabled, got)
		}
		// One more step: approved, still enabled as before, setup to apply.
		moreSetup := func(s *extsign.Spec) {
			withSetup(s)
			s.Manifest.HostSetup = append(s.Manifest.HostSetup, extapi.HostSetupStep{Type: extapi.HostSetupUSBDevice, VendorID: "1234", ProductID: "abce"})
		}
		raw := pkg(t, w, "1.2.0", moreSetup)
		_, err = h.Upgrade(bytes.NewReader(raw), extpkg.UpgradeOptions{})
		_, need := approvalOf(t, err)
		if !slices.Equal(need.Diff.HostSetupAdded, []string{"usb_device 1234:abce"}) {
			t.Fatalf("diff = %+v", need.Diff)
		}
		if _, err := h.Upgrade(bytes.NewReader(raw), extpkg.UpgradeOptions{Approve: need.Hash, ApprovedBy: "ops"}); err != nil {
			t.Fatal(err)
		}
		got, _, _ = h.Dir.ReadControl("acme_backup")
		if got.Enabled != enabled || got.HostSetupApplied || got.ApprovedBy != "ops" {
			t.Fatalf("enabled=%v: control after 1.2.0 = %+v", enabled, got)
		}
	}
}

// An upgrade needs the package installed: otherwise it is refused before
// approval and unpacks nothing.
func TestUpgradeRefusesAPackageThatIsNotInstalled(t *testing.T) {
	w := newWorld(t)
	h := upgradeHost(t, w, true)
	_, err := h.Upgrade(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.UpgradeOptions{})
	if code, params := codeOf(err); code != messages.ErrorPluginNotInstalled || params["id"] != "acme_backup" {
		t.Fatalf("upgrade of a package not installed = %v %v", err, params)
	}
	if versions, _ := h.Dir.Versions("acme_backup"); len(versions) != 0 {
		t.Fatalf("a refused upgrade left versions %v", versions)
	}
	// Removed (data kept) counts as not installed too.
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Remove("acme_backup", false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Upgrade(bytes.NewReader(pkg(t, w, "1.1.0", nil)), extpkg.UpgradeOptions{}); !isCode(err, messages.ErrorPluginNotInstalled) {
		t.Fatalf("upgrade after remove = %v", err)
	}
}

// An installed version that no longer verifies is diffed as nothing, so
// the administrator sees everything the new one asks for.
func TestUpgradeOverABrokenVersionShowsTheFullAsk(t *testing.T) {
	w := newWorld(t)
	h := upgradeHost(t, w, false)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", v1Asks)), extpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	h.Verifier.Offered = map[string]int{"host.setup": 1} // 1.0.0 needs routes, no longer offered
	if _, err := h.Recheck("acme_backup", false); err == nil {
		t.Fatal("1.0.0 still verifies")
	}
	_, err := h.Upgrade(bytes.NewReader(pkg(t, w, "2.0.0", nil)), extpkg.UpgradeOptions{})
	_, need := approvalOf(t, err)
	if need.Diff.FromVersion != "1.0.0" || len(need.Diff.PermissionsRemoved) != 0 || need.Diff.Isolation.From != "" {
		t.Fatalf("diff over a broken version = %+v", need.Diff)
	}
}

// HostConfig carries the policy's auto-approval into the verifier policy.
func TestHostConfigCarriesAutoApproval(t *testing.T) {
	for _, auto := range []bool{false, true} {
		_, p, err := extpkg.HostConfig(serverconfig.Plugins{Policy: serverconfig.PluginPolicy{AutoApproveUnchanged: auto}})
		if err != nil || p.AutoApproveUnchanged != auto {
			t.Fatalf("auto %v: policy %+v, %v", auto, p, err)
		}
	}
}

// Installing over an enabled version without Enable keeps it enabled, and
// a disabled one stays disabled; a first install takes Enable as given.
func TestInstallOverAnInstalledVersionKeepsEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint("enabled=", enabled), func(t *testing.T) {
			w := newWorld(t)
			h := upgradeHost(t, w, false)
			if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", v1Asks)), extpkg.InstallOptions{Enable: enabled}); err != nil {
				t.Fatal(err)
			}
			in, err := h.Install(bytes.NewReader(pkg(t, w, "2.0.0", v2Asks)), extpkg.InstallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ctl, _, _ := h.Dir.ReadControl("acme_backup")
			if ctl.Enabled != enabled || in.Enabled != enabled {
				t.Fatalf("control enabled = %v, installed enabled = %v, want %v", ctl.Enabled, in.Enabled, enabled)
			}
			// The same build again keeps it too.
			if in, err = h.Install(bytes.NewReader(pkg(t, w, "2.0.0", v2Asks)), extpkg.InstallOptions{}); err != nil || !in.Unchanged {
				t.Fatalf("reinstall = %+v, %v", in, err)
			}
			if ctl, _, _ := h.Dir.ReadControl("acme_backup"); ctl.Enabled != enabled {
				t.Fatalf("control enabled = %v after the same build, want %v", ctl.Enabled, enabled)
			}
		})
	}
}

// An install over an installed version is refused with the diff from that
// version, the same diff an upgrade shows.
func TestInstallOverAnInstalledVersionDiffsFromIt(t *testing.T) {
	w := newWorld(t)
	h := upgradeHost(t, w, false)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", v1Asks)), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	_, err := h.Install(bytes.NewReader(pkg(t, w, "2.0.0", v2Asks)), extpkg.InstallOptions{RequireApproval: true})
	params, need := approvalOf(t, err)
	_, upErr := h.Upgrade(bytes.NewReader(pkg(t, w, "2.0.0", v2Asks)), extpkg.UpgradeOptions{})
	upParams, upNeed := approvalOf(t, upErr)
	wantDiff := upNeed.Diff
	wantDiff.HashChanged = false // Install does not compare with the approved hash.
	if !reflect.DeepEqual(need.Diff, wantDiff) || need.Diff.FromVersion != "1.0.0" {
		t.Fatalf("install diff = %+v\nupgrade diff = %+v", need.Diff, upNeed.Diff)
	}
	if !reflect.DeepEqual(params, upParams) {
		t.Fatalf("install params = %v\nupgrade params = %v", params, upParams)
	}
	if msg := need.Error(); msg != fmt.Sprintf("acme_backup 2.0.0 needs approval %s", need.Hash) {
		t.Fatalf("cause = %q", msg)
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current = %q after a refused install", v)
	}
}
