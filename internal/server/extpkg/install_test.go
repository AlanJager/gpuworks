package extpkg_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// memMarkers is an in-memory marker table with injectable failures.
type memMarkers struct {
	mu        sync.Mutex
	markers   map[string]extapi.Marker
	readErr   error
	setErr    error
	deleteErr error
	sets      int
}

func (m *memMarkers) PluginMarkers() ([]extapi.Marker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return nil, m.readErr
	}
	var out []extapi.Marker
	for _, v := range m.markers {
		out = append(out, v)
	}
	return out, nil
}

func (m *memMarkers) SetPluginMarker(v extapi.Marker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return m.setErr
	}
	m.sets++
	m.markers[v.Name] = v
	return nil
}

func (m *memMarkers) DeletePluginMarker(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.markers, name)
	return nil
}

func newHost(t *testing.T, w *world) (*extpkg.Host, *memMarkers) {
	t.Helper()
	mk := &memMarkers{markers: map[string]extapi.Marker{}}
	dataDir := t.TempDir()
	t.Cleanup(func() { layout.RemoveAll(dataDir) })
	h := &extpkg.Host{Dir: layout.New(dataDir), Verifier: w.verifier(), Markers: mk, Hasher: layout.NewHasher(),
		Now: func() time.Time { return extsign.Epoch.Add(48 * time.Hour) }}
	return h, mk
}

func pkg(t *testing.T, w *world, version string, edit func(*extsign.Spec)) []byte {
	t.Helper()
	s := extsign.Minimal("acme", "acme_backup", version)
	if edit != nil {
		edit(&s)
	}
	return extsign.Package(t, w.pub, s)
}

func TestInstallUnpacksReadOnlyAndActivates(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	in, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{Enable: true, ApprovedBy: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Manifest.Version != "1.0.0" || in.Previous != "" || in.Unchanged || !in.ApprovalHash.Valid() {
		t.Fatalf("installed = %+v", in)
	}
	dir := h.Dir.Version("acme_backup", "1.0.0")
	for name, mode := range map[string]os.FileMode{
		"bin/linux_amd64/acme_backup": 0o555, "locales/en.json": 0o444, "manifest.json": 0o444, "manifest.sig": 0o444, "publisher.crt": 0o444, "bin": 0o555 | os.ModeDir,
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode() != mode {
			t.Fatalf("%s: mode %v err %v, want %v", name, info.Mode(), err, mode)
		}
	}
	for _, d := range []string{h.Dir.Data("acme_backup"), h.Dir.Tmp("acme_backup"), h.Dir.Snapshots("acme_backup")} {
		if info, err := os.Stat(d); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, info, err)
		}
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current = %q", v)
	}
	ctl, ok, err := h.Dir.ReadControl("acme_backup")
	if err != nil || !ok || !ctl.Enabled || ctl.ApprovedHash != string(in.ApprovalHash) || ctl.ApprovedBy != "root" {
		t.Fatalf("control = %+v %v %v", ctl, ok, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(h.Dir.Root, ".incoming-*")); len(leftovers) != 0 {
		t.Fatalf("incoming directories left: %v", leftovers)
	}
}

func TestInstallSameBuildAgainIsUnchanged(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	raw := pkg(t, w, "1.0.0", nil)
	if _, err := h.Install(bytes.NewReader(raw), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	in, err := h.Install(bytes.NewReader(raw), extpkg.InstallOptions{Enable: false, ApprovedBy: "ops"})
	if err != nil || !in.Unchanged || in.Previous != "" {
		t.Fatalf("reinstall = %+v %v", in, err)
	}
	// The new control is applied, and the package stays enabled.
	if ctl, _, _ := h.Dir.ReadControl("acme_backup"); ctl.ApprovedBy != "ops" || !ctl.Enabled {
		t.Fatalf("control after reinstall = %+v", ctl)
	}
}

func TestInstallRefusesAnotherBuildUnderAnInstalledVersion(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	other := pkg(t, w, "1.0.0", func(s *extsign.Spec) { s.Files["locales/zh.json"] = extsign.File{Body: []byte("{}")} })
	_, err := h.Install(bytes.NewReader(other), extpkg.InstallOptions{})
	if code, _ := codeOf(err); code != messages.ErrorPluginVersionConflict {
		t.Fatalf("conflict = %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.Dir.Version("acme_backup", "1.0.0"), "locales/zh.json")); !os.IsNotExist(err) {
		t.Fatal("the conflicting build reached the version directory")
	}
}

func TestUpgradesKeepThePreviousVersionOnly(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	for i, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		in, err := h.Install(bytes.NewReader(pkg(t, w, v, nil)), extpkg.InstallOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && in.Previous == "" {
			t.Fatalf("%s: previous not reported", v)
		}
	}
	versions, _ := h.Dir.Versions("acme_backup")
	if len(versions) != 2 || versions[0] != "1.1.0" || versions[1] != "1.2.0" {
		t.Fatalf("versions = %v", versions)
	}
	// Rolling back to the previous build is an install of it.
	if _, err := h.Dir.Activate("acme_backup", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "1.1.0" {
		t.Fatalf("current after rollback = %s", v)
	}
}

func TestRefusedPackagesLeaveNothingInPlace(t *testing.T) {
	w := newWorld(t)
	h, mk := newHost(t, w)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		raw  []byte
		code string
	}{
		"malformed": {[]byte("not a package"), messages.ErrorPluginPackageMalformed},
		"unsigned by a trusted key": {extsign.Package(t, extsign.NewPublisher(t, "acme", nil, extsign.NewRoot(t, "x")), extsign.Minimal("acme", "acme_backup", "2.0.0")),
			extapi.ErrorSignatureUntrusted},
		"incompatible": {pkg(t, w, "2.0.0", func(s *extsign.Spec) { s.Manifest.Requires = map[string]int{"node.app": 1} }), messages.ErrorPluginIncompatible},
	}
	for name, c := range cases {
		_, err := h.Install(bytes.NewReader(c.raw), extpkg.InstallOptions{})
		if code, _ := codeOf(err); code != c.code {
			t.Fatalf("%s: err = %v, want %s", name, err, c.code)
		}
	}
	// A data marker newer than the build's storage refuses the build.
	mk.markers[extapi.PluginDataMarker("acme_backup")] = extapi.Marker{Name: extapi.PluginDataMarker("acme_backup"), Version: 3, WrittenBy: "v27.1.0"}
	older := pkg(t, w, "2.0.0", func(s *extsign.Spec) {
		s.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: 2}
	})
	_, err := h.Install(bytes.NewReader(older), extpkg.InstallOptions{})
	if code, params := codeOf(err); code != extapi.ErrorStateTooNew || params["needs"] != "v27.1.0" {
		t.Fatalf("older than its data = %v %v", err, params)
	}
	mk.readErr = errors.New("database is locked")
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "2.0.0", nil)), extpkg.InstallOptions{}); !errors.Is(err, mk.readErr) {
		t.Fatalf("marker read failure = %v", err)
	}

	if versions, _ := h.Dir.Versions("acme_backup"); len(versions) != 1 || versions[0] != "1.0.0" {
		t.Fatalf("versions after refusals = %v", versions)
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current after refusals = %s", v)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(h.Dir.Root, ".incoming-*")); len(leftovers) != 0 {
		t.Fatalf("incoming directories left: %v", leftovers)
	}
	if ctl, _, _ := h.Dir.ReadControl("acme_backup"); !ctl.Enabled {
		t.Fatal("a refused install changed the control file")
	}
}

func TestCleanupRemovesInterruptedInstalls(t *testing.T) {
	d := layout.New(t.TempDir())
	s, err := d.Stage()
	if err != nil {
		t.Fatal(err)
	}
	if w, err := s.Create("bin/x"); err != nil {
		t.Fatal(err)
	} else {
		w.Close()
	}
	partial := filepath.Join(d.Root, "acme_x", "pkg", "1.0.0.partial")
	if err := os.MkdirAll(filepath.Join(partial, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(partial, 0o555)
	if err := d.Cleanup(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.Path(), partial} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived cleanup", p)
		}
	}
	if _, err := s.Create("../escape"); err == nil {
		t.Fatal("staging accepted an unsafe path")
	}
}

func makeWritable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestRecheckDetectsTamperingAndCachesHashes(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	hasher := layout.NewHasher()
	h.Hasher = hasher
	if _, err := h.Recheck("acme_backup", true); err != nil {
		t.Fatal(err)
	}
	full := hasher.Hashed()
	if full == 0 {
		t.Fatal("full recheck hashed nothing")
	}
	if _, err := h.Recheck("acme_backup", false); err != nil {
		t.Fatal(err)
	}
	if hasher.Hashed() != full {
		t.Fatalf("an unchanged package was rehashed (%d -> %d)", full, hasher.Hashed())
	}
	if _, err := h.Recheck("acme_backup", true); err != nil || hasher.Hashed() != 2*full {
		t.Fatalf("full recheck used the cache: %v %d", err, hasher.Hashed())
	}

	dir := h.Dir.Version("acme_backup", "1.0.0")
	locale := filepath.Join(dir, "locales", "en.json")
	makeWritable(t, locale)
	if err := os.WriteFile(locale, []byte(`[]`), 0o444); err != nil {
		t.Fatal(err)
	}
	// Same size, new modification time: the quick check rehashes it.
	os.Chtimes(locale, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	_, err := h.Recheck("acme_backup", false)
	if code, params := codeOf(err); code != messages.ErrorPluginFileMismatch || params["reason"] != extpkg.FileSHA256 {
		t.Fatalf("tampered file = %v %v", err, params)
	}
	list, err := h.List()
	if err != nil || len(list) != 1 || list[0].Err == nil || list[0].Verified != nil {
		t.Fatalf("list of a broken package = %+v %v", list, err)
	}
}

func TestRecheckRefusesExtraFilesAndLinks(t *testing.T) {
	for name, plant := range map[string]func(dir string) error{
		"extra file": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "locales", "extra.json"), []byte("{}"), 0o444)
		},
		"symlink": func(dir string) error { return os.Symlink("/etc/passwd", filepath.Join(dir, "locales", "zz")) },
		"missing signature": func(dir string) error {
			return os.Remove(filepath.Join(dir, "manifest.sig"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			h, _ := newHost(t, w)
			if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{}); err != nil {
				t.Fatal(err)
			}
			dir := h.Dir.Version("acme_backup", "1.0.0")
			os.Chmod(dir, 0o755)
			os.Chmod(filepath.Join(dir, "locales"), 0o755)
			if err := plant(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Recheck("acme_backup", true); err == nil {
				t.Fatal("recheck passed")
			}
		})
	}
}

func TestRemove(t *testing.T) {
	w := newWorld(t)
	h, mk := newHost(t, w)
	if err := h.Remove("acme_backup", false); func() string { c, _ := codeOf(err); return c }() != messages.ErrorPluginNotInstalled {
		t.Fatalf("remove of nothing = %v", err)
	}
	if err := h.Remove("../etc", true); func() string { c, _ := codeOf(err); return c }() != messages.ErrorPluginNotInstalled {
		t.Fatalf("remove of a path = %v", err)
	}
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(h.Dir.Data("acme_backup"), "state.db")
	if err := os.WriteFile(data, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := extapi.PluginDataMarker("acme_backup")
	mk.markers[marker] = extapi.Marker{Name: marker, Version: 1}
	if err := h.Remove("acme_backup", false); err != nil {
		t.Fatal(err)
	}
	if ids, _ := h.Dir.Installed(); len(ids) != 0 {
		t.Fatalf("still installed: %v", ids)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatal("remove without purge dropped the plugin's data")
	}
	if _, ok := mk.markers[marker]; !ok {
		t.Fatal("remove without purge dropped the data marker")
	}
	mk.deleteErr = errors.New("database is locked")
	if err := h.Remove("acme_backup", true); !errors.Is(err, mk.deleteErr) {
		t.Fatalf("marker delete failure = %v", err)
	}
	mk.deleteErr = nil
	// Purging again finishes the interrupted purge: the marker goes.
	if err := h.Remove("acme_backup", true); err != nil {
		t.Fatalf("finishing the purge = %v", err)
	}
	if _, ok := mk.markers[marker]; ok {
		t.Fatal("the retried purge kept the data marker")
	}
	if err := h.Remove("acme_backup", true); func() string { c, _ := codeOf(err); return c }() != messages.ErrorPluginNotInstalled {
		t.Fatalf("purge of nothing = %v", err)
	}
	mk.markers[marker] = extapi.Marker{Name: marker, Version: 1}
	// The kept data marker still guards the data: a build that writes the
	// same data version installs over it.
	withData := func(s *extsign.Spec) {
		s.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: 1}
	}
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.1", withData)), extpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Remove("acme_backup", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.Dir.Package("acme_backup")); !os.IsNotExist(err) {
		t.Fatal("purge kept the package directory")
	}
	trash, _ := filepath.Glob(filepath.Join(h.Dir.Root, ".trash", "acme_backup-*"))
	if len(trash) != 2 {
		t.Fatalf("trash = %v", trash)
	}
	if _, ok := mk.markers[marker]; ok {
		t.Fatal("purge kept the data marker")
	}
}

func TestConcurrentInstallsSerialize(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	raws := [][]byte{pkg(t, w, "1.0.0", nil), pkg(t, w, "1.1.0", nil), pkg(t, w, "1.2.0", nil)}
	var wg sync.WaitGroup
	errs := make([]error, len(raws))
	for i, raw := range raws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = h.Install(bytes.NewReader(raw), extpkg.InstallOptions{})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Recheck("acme_backup", true); err != nil {
		t.Fatalf("after concurrent installs: %v", err)
	}
	if versions, _ := h.Dir.Versions("acme_backup"); len(versions) != 2 {
		t.Fatalf("versions = %v", versions)
	}
}

// featureOwners is a fixed owner per key; it records which package asked.
type featureOwners struct {
	owners map[string]string
	asked  []string
}

func (f *featureOwners) FeatureOwner(id, key string) string {
	f.asked = append(f.asked, id+" "+key)
	if owner := f.owners[key]; owner != id {
		return owner
	}
	return ""
}

// A package declaring a key someone else provides is refused before
// approval is asked for, naming the key and its owner, and nothing is
// unpacked; the package's own keys and free keys install.
func TestInstallRefusesAFeatureKeySomeoneElseProvides(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	withFeatures := func(s *extsign.Spec) {
		s.Manifest.Features = []extapi.FeatureDecl{
			{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf},
			{Key: "acme.tools", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf},
		}
	}
	raw := pkg(t, w, "1.0.0", withFeatures)
	for owner, want := range map[string]string{"acme_other": "acme_other", "ops_audit": "ops_audit"} {
		owners := &featureOwners{owners: map[string]string{"acme.tools": owner}}
		_, err := h.Install(bytes.NewReader(raw), extpkg.InstallOptions{RequireApproval: true, Features: owners})
		code, params := codeOf(err)
		if code != messages.ErrorPluginFeatureTaken || params["id"] != "acme_backup" || params["feature"] != "acme.tools" || params["owner"] != want {
			t.Fatalf("install over %s's key = %v %v", owner, err, params)
		}
		if versions, _ := h.Dir.Versions("acme_backup"); len(versions) != 0 {
			t.Fatalf("a refused install left versions %v", versions)
		}
	}
	// The package that already owns its keys (an upgrade) installs.
	owners := &featureOwners{owners: map[string]string{"acme.backup": "acme_backup", "acme.tools": "acme_backup"}}
	if _, err := h.Install(bytes.NewReader(raw), extpkg.InstallOptions{Features: owners}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme_backup acme.backup", "acme_backup acme.tools"}; !slices.Equal(owners.asked, want) {
		t.Fatalf("asked %v, want %v", owners.asked, want)
	}
}
