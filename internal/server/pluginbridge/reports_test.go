package pluginbridge

import (
	"context"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg/catalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// uninstallConfirmed previews package id's uninstall and confirms it with
// the digest it got, as the console does.
func uninstallConfirmed(t *testing.T, b *Bridge, ctx context.Context, id string, purge bool) error {
	t.Helper()
	im, err := b.Impact(id)
	if err != nil {
		return err
	}
	_, err = b.Uninstall(ctx, id, UninstallRequest{Purge: purge, Confirm: im.Digest, By: "admin"})
	return err
}

// importIndex imports an index of acme listing entries, as `gpuworks plugin
// index import` does.
func (w *world) importIndex(b *Bridge, serial uint64, entries ...extapi.IndexEntry) {
	w.t.Helper()
	idx := extsign.Index(w.t, w.pub, serial, entries, nil)
	if _, err := catalog.Import(w.st, b.pkgs.Verifier.Trust, b.pkgs.Dir, idx, w.pub.CertBundle(w.t)); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) indexEntry(id, version string, requires map[string]int) extapi.IndexEntry {
	w.t.Helper()
	spec := extsign.Minimal("acme", id, version)
	spec.Manifest.Requires = requires
	spec.Manifest.MinCoreHint = "v27.3"
	built := extsign.Build(w.t, w.pub, spec)
	return built.IndexEntry(w.t, id+"_"+version+".gwpkg", built.Archive(w.t))
}

// The catalog joins the imported index with the installed packages: the
// newest build this server can run is the best one, newer builds it cannot
// run are refused with their code, an installed package the index does
// not list is still in it, and an installed one with a newer best build
// shows it as pending in its view.
func TestCatalogChoosesTheBestBuildAndListsRefusedNewerOnes(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	install(t, b, w.pkg("acme_off", "1.0.0", nil), false)
	w.importIndex(b, 1,
		w.indexEntry("acme_backup", "1.0.0", nil),
		w.indexEntry("acme_backup", "1.1.0", nil),
		w.indexEntry("acme_backup", "2.0.0", map[string]int{"host.notify": 1}),
		w.indexEntry("acme_vault", "3.0.0", nil))
	c, err := b.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(c.Entries))
	for i, e := range c.Entries {
		ids[i] = e.ID
	}
	if !slices.Equal(ids, []string{"acme_backup", "acme_off", "acme_vault"}) || len(c.Errors) != 0 {
		t.Fatalf("catalog = %+v", c)
	}
	backup, off, vault := c.Entries[0], c.Entries[1], c.Entries[2]
	if backup.Best == nil || backup.Best.Version != "1.1.0" || backup.Installed != "1.0.0" || !backup.Installable ||
		len(backup.Refused) != 1 || backup.Refused[0].Version != "2.0.0" || backup.Refused[0].Code != messages.ErrorPluginNeedsNewerCore ||
		backup.Refused[0].Params["capability"] != "host.notify" || backup.Refused[0].MinCoreHint != "v27.3" {
		t.Fatalf("acme_backup = %+v", backup)
	}
	if off.Best != nil || off.Installable || off.Installed != "1.0.0" || off.Publisher != "acme" {
		t.Fatalf("acme_off (installed, not listed) = %+v", off)
	}
	if vault.Best == nil || vault.Best.Version != "3.0.0" || !vault.Installable || vault.Installed != "" {
		t.Fatalf("acme_vault = %+v", vault)
	}
	if v := b.view("acme_backup"); v.Pending != "1.1.0" {
		t.Fatalf("pending of acme_backup = %q", v.Pending)
	}
	if v := b.view("acme_off"); v.Pending != "" {
		t.Fatalf("pending of acme_off = %q", v.Pending)
	}
	// Installing the best build leaves nothing pending; the version it
	// replaced is kept for rollback.
	install(t, b, w.pkg("acme_backup", "1.1.0", nil), false)
	if v := b.view("acme_backup"); v.Pending != "" || v.Previous != "1.0.0" {
		t.Fatalf("after installing the best build: pending %q previous %q", v.Pending, v.Previous)
	}
}

// A view lists the keys the package declares with their access now.
func TestViewListsTheDeclaredFeaturesWithTheirAccess(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", func(s *extsign.Spec) {
		s.Manifest.Features = []extapi.FeatureDecl{
			{Key: "acme.tools", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf},
			{Key: "acme.free", Presentation: extapi.PresentationPromote, Gate: extapi.GateNone},
		}
	}), false)
	v := b.view("acme_backup")
	if !slices.Equal(v.Features, []FeatureView{{Key: "acme.free", Access: "full"}, {Key: "acme.tools", Access: "locked"}}) || v.OnDemand {
		t.Fatalf("view = %+v", v)
	}
}

func TestPreviousIsTheNewestOlderVersion(t *testing.T) {
	for _, tc := range []struct {
		current  string
		versions []string
		want     string
	}{
		{"1.0.0", []string{"1.0.0"}, ""},
		{"1.2.0", []string{"1.0.0", "1.2.0", "1.1.0"}, "1.1.0"},
		{"1.1.0", []string{"1.0.0", "1.2.0", "1.1.0"}, "1.0.0"},
		{"1.0.0", nil, ""},
	} {
		if got := previousOf(tc.current, tc.versions); got != tc.want {
			t.Errorf("previousOf(%s, %v) = %q, want %q", tc.current, tc.versions, got, tc.want)
		}
	}
}
