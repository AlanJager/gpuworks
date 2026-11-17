package cli

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

type pluginEnv struct {
	t      *testing.T
	opt    Options
	root   *extsign.Key
	pub    *extsign.Publisher
	data   string
	config string
}

func newPluginEnv(t *testing.T, serverYAML string) *pluginEnv {
	t.Helper()
	root := extsign.NewRoot(t, "1")
	dir := t.TempDir()
	t.Cleanup(func() { layout.RemoveAll(dir) })
	e := &pluginEnv{
		t: t, root: root, pub: extsign.NewPublisher(t, "acme", nil, root),
		data:   filepath.Join(dir, "data"),
		config: filepath.Join(dir, "server.yaml"),
		opt:    Options{Distribution: distribution.Profile{PluginRoots: []extapi.PublicKey{root.Public}}},
	}
	if err := os.WriteFile(e.config, []byte(serverYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.data, 0o700); err != nil {
		t.Fatal(err)
	}
	return e
}

// run runs `gpuworks plugin args...` against the environment's data
// directory and server.yaml, and returns what it printed.
func (e *pluginEnv) run(args ...string) (string, error) {
	e.t.Helper()
	var out bytes.Buffer
	args = append(args, "--data", e.data, "--config", e.config)
	err := e.opt.pluginCommand(slog.New(slog.NewTextHandler(io.Discard, nil)), args, &out)
	return out.String(), err
}

func (e *pluginEnv) writePackage(name string, raw []byte) string {
	e.t.Helper()
	path := filepath.Join(e.t.TempDir(), name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func productCode(err error) string {
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code()
	}
	return extapi.CodeOf(err)
}

func TestPluginInstallListRemoveWithTheServerStopped(t *testing.T) {
	e := newPluginEnv(t, "")
	out, err := e.run("list")
	if err != nil || !strings.Contains(out, "no plugin installed") {
		t.Fatalf("empty list = %q %v", out, err)
	}
	v1 := e.writePackage("v1.gwpkg", extsign.Package(t, e.pub, extsign.Minimal("acme", "acme_backup", "1.0.0")))
	out, err = e.run("install", "--file", v1)
	if err != nil || !strings.Contains(out, "installed acme_backup 1.0.0 (publisher acme, project trust)") {
		t.Fatalf("install = %q %v", out, err)
	}
	v2 := e.writePackage("v2.gwpkg", extsign.Package(t, e.pub, extsign.Minimal("acme", "acme_backup", "1.1.0")))
	out, err = e.run("install", "--disabled", "--file", v2)
	if err != nil || !strings.Contains(out, "previous version 1.0.0 kept for rollback") {
		t.Fatalf("upgrade = %q %v", out, err)
	}
	out, err = e.run("list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ID", "acme_backup", "1.1.0", "acme", "project", "false", "ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list lacks %q:\n%s", want, out)
		}
	}
	// A file changed in place shows as broken with its code.
	locale := filepath.Join(e.data, "plugins", "acme_backup", "pkg", "1.1.0", "locales", "en.json")
	os.Chmod(filepath.Dir(locale), 0o755)
	os.Chmod(locale, 0o644)
	if err := os.WriteFile(locale, []byte(`{"x":1}`), 0o444); err != nil {
		t.Fatal(err)
	}
	if out, _ := e.run("list"); !strings.Contains(out, "broken: "+messages.ErrorPluginFileMismatch) {
		t.Fatalf("broken package not shown:\n%s", out)
	}

	if _, err := e.run("remove", "acme_backup"); err != nil {
		t.Fatal(err)
	}
	if out, _ := e.run("list"); !strings.Contains(out, "no plugin installed") {
		t.Fatalf("after remove:\n%s", out)
	}
	if _, err := e.run("remove", "acme_backup"); productCode(err) != messages.ErrorPluginNotInstalled {
		t.Fatalf("second remove = %v", err)
	}
	if _, err := e.run("install"); err == nil {
		t.Fatal("install without --file succeeded")
	}
	if _, err := e.run("remove"); err == nil {
		t.Fatal("remove without an id succeeded")
	}
}

func TestPluginInstallRefusesWithCodes(t *testing.T) {
	e := newPluginEnv(t, "")
	stranger := extsign.NewPublisher(t, "acme", nil, extsign.NewRoot(t, "9"))
	path := e.writePackage("x.gwpkg", extsign.Package(t, stranger, extsign.Minimal("acme", "acme_backup", "1.0.0")))
	if _, err := e.run("install", "--file", path); productCode(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("untrusted = %v", err)
	}
	// The production build compiles no root: nothing installs without one.
	e.opt.Distribution = distribution.Profile{}
	good := e.writePackage("y.gwpkg", extsign.Package(t, e.pub, extsign.Minimal("acme", "acme_backup", "1.0.0")))
	if _, err := e.run("install", "--file", good); productCode(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("install without a compiled root = %v", err)
	}
}

func TestPluginTrustImportRaisesTheFloor(t *testing.T) {
	e := newPluginEnv(t, "")
	update := extsign.TrustUpdate(t, 1, func(u *extapi.TrustUpdate) {
		u.Floors = []extapi.Floor{{ID: "acme_backup", MinVersion: "1.1.0"}}
	}, e.root)
	file := e.writePackage("trust.json", update)
	out, err := e.run("trust", "import", file)
	if err != nil || !strings.Contains(out, "applied trust_update serial 1") || !strings.Contains(out, "1 floors") {
		t.Fatalf("trust import = %q %v", out, err)
	}
	if _, err := e.run("trust", "import", file); productCode(err) != messages.ErrorPluginTrustReplayed {
		t.Fatalf("replay = %v", err)
	}
	old := e.writePackage("old.gwpkg", extsign.Package(t, e.pub, extsign.Minimal("acme", "acme_backup", "1.0.0")))
	if _, err := e.run("install", "--file", old); productCode(err) != messages.ErrorPluginBelowFloor {
		t.Fatalf("below the floor = %v", err)
	}
	crl := e.writePackage("crl.json", extsign.CRL(t, 1, "acme", []extapi.Revocation{{Publisher: "acme", CertSerial: 1}}, e.pub.Key))
	cert := e.writePackage("publisher.crt", e.pub.CertBundle(t))
	if out, err := e.run("trust", "import", "--cert", cert, crl); err != nil || !strings.Contains(out, "applied crl serial 1") {
		t.Fatalf("publisher crl = %q %v", out, err)
	}
	fresh := e.writePackage("new.gwpkg", extsign.Package(t, e.pub, extsign.Minimal("acme", "acme_backup", "1.2.0")))
	if _, err := e.run("install", "--file", fresh); productCode(err) != messages.ErrorPluginCertRevoked {
		t.Fatalf("revoked certificate = %v", err)
	}
	if _, err := e.run("trust", "frobnicate"); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("unknown trust verb = %v", err)
	}
}

func TestPluginHostTrustedIssuerFromServerYAML(t *testing.T) {
	issuer := extsign.NewKey(t, "site-issuer")
	yaml := "plugins:\n  publishers:\n    - keyid: site-issuer\n      alg: ed25519\n      public_key: " +
		base64.StdEncoding.EncodeToString(issuer.Public.Key) + "\n  allow_unsandboxed: [beta_tools]\n"
	e := newPluginEnv(t, yaml)
	e.opt.Distribution = distribution.Profile{}
	pub := extsign.NewPublisher(t, "beta", nil, issuer)
	path := e.writePackage("b.gwpkg", extsign.Package(t, pub, extsign.Minimal("beta", "beta_tools", "1.0.0")))
	out, err := e.run("install", "--file", path)
	if err != nil {
		t.Fatal(err)
	}
	// allow_unsandboxed keeps the verdict the same on hosts without Landlock.
	if !strings.Contains(out, "host trust") {
		t.Fatalf("install = %q", out)
	}
	if err := os.WriteFile(e.config, []byte("plugins:\n  policy:\n    allow_publishers: [acme]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := e.writePackage("c.gwpkg", extsign.Package(t, pub, extsign.Minimal("beta", "beta_tools", "1.0.1")))
	if _, err := e.run("install", "--file", other); productCode(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("issuer removed from server.yaml = %v", err)
	}
	if err := os.WriteFile(e.config, []byte("plugins:\n  bogus: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run("list"); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("broken server.yaml = %v", err)
	}
}

func TestPluginCompatIsReadOnly(t *testing.T) {
	e := newPluginEnv(t, "")
	out, err := e.run("compat")
	if err != nil || !strings.Contains(out, "compatible") {
		t.Fatalf("fresh data directory = %q %v", out, err)
	}
	spec := extsign.Minimal("acme", "acme_backup", "1.0.0")
	spec.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: 1}
	if _, err := e.run("install", "--file", e.writePackage("a.gwpkg", extsign.Package(t, e.pub, spec))); err != nil {
		t.Fatal(err)
	}
	if out, err := e.run("compat"); err != nil || !strings.Contains(out, "1 installed plugins") {
		t.Fatalf("compatible = %q %v", out, err)
	}
	db := filepath.Join(e.data, "gpuworks.db")
	st, err := store.Open(db, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []extapi.Marker{
		{Name: "pstate.availability", Version: 2, WrittenBy: "v27.4.0"},
		{Name: extapi.PluginDataMarker("acme_backup"), Version: 2, WrittenBy: "v27.4.0"},
	} {
		if err := st.SetPluginMarker(m); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	before, _ := os.ReadFile(db)
	out, err = e.run("compat")
	if !errors.Is(err, errCompatFindings) {
		t.Fatalf("compat with newer state = %v", err)
	}
	for _, want := range []string{"refuses to start: pstate.availability", extapi.ErrorStateTooNew, "plugin will not run: acme_backup"} {
		if !strings.Contains(out, want) {
			t.Fatalf("compat output lacks %q:\n%s", want, out)
		}
	}
	if after, _ := os.ReadFile(db); !bytes.Equal(before, after) {
		t.Fatal("compat wrote to the database")
	}
}

func TestPluginNeedsAnExistingDataDirectory(t *testing.T) {
	e := newPluginEnv(t, "")
	e.data += "-typo"
	if _, err := e.run("list"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing data directory = %v", err)
	}
	if _, err := os.Stat(e.data); !os.IsNotExist(err) {
		t.Fatal("a missing data directory was created")
	}
}

func TestRunDispatchesPlugin(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var stderr bytes.Buffer
	if code := Run(log, []string{"plugin", "frobnicate"}, &stderr, Options{}); code != 1 {
		t.Fatalf("unknown plugin subcommand exit = %d", code)
	}
	if code := Run(log, []string{"plugin", "help"}, &stderr, Options{}); code != 0 {
		t.Fatalf("plugin help exit = %d", code)
	}
}
