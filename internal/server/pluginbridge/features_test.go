package pluginbridge

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// declaring makes a package declare keys, answered by itself.
func declaring(keys ...string) func(*extsign.Spec) {
	return func(s *extsign.Spec) {
		for _, k := range keys {
			s.Manifest.Features = append(s.Manifest.Features, extapi.FeatureDecl{Key: k, Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf})
		}
	}
}

func takenBy(t *testing.T, err error, id, feature, owner string) {
	t.Helper()
	code, params, _ := codeOf(err)
	if code != messages.ErrorPluginFeatureTaken || params["id"] != id || params["feature"] != feature || params["owner"] != owner {
		t.Fatalf("install = %v %v, want %s taken by %s", err, params, feature, owner)
	}
}

// A package installed while the server runs declares its keys at once;
// one declaring a key another package or a compiled module provides is
// refused before approval and changes nothing; an upgrade keeps its own
// keys; an uninstall withdraws them. Each change is announced.
func TestFeatureKeysFollowInstallsAndKeepTheirOwner(t *testing.T) {
	w := newWorld(t, nil)
	w.static = []featuregate.Feature{{Key: "acme.vault"}}
	b := w.open()
	ctx := context.Background()
	install(t, b, w.pkg("acme_backup", "1.0.0", declaring("acme.backup")), false)
	if !w.gate.Declared("acme.backup") {
		t.Fatal("acme.backup was not declared at install")
	}

	before := w.announced.Load()
	_, err := b.Install(ctx, bytes.NewReader(w.pkg("acme_off", "1.0.0", declaring("acme.off", "acme.backup"))), InstallRequest{By: "admin"})
	takenBy(t, err, "acme_off", "acme.backup", "acme_backup")
	_, err = b.Install(ctx, bytes.NewReader(w.pkg("acme_driver", "1.0.0", declaring("acme.vault"))), InstallRequest{By: "admin"})
	takenBy(t, err, "acme_driver", "acme.vault", "ops")
	if len(b.List()) != 1 || w.gate.Declared("acme.off") || w.announced.Load() != before {
		t.Fatalf("refused installs changed something: %v, announced %d → %d", b.List(), before, w.announced.Load())
	}
	if got := w.gate.States(); len(got) != 2 || got[0].Key != "acme.backup" || got[1].Key != "acme.vault" {
		t.Fatalf("states = %+v", got)
	}

	install(t, b, w.pkg("acme_backup", "1.0.1", declaring("acme.backup", "acme.tools")), false)
	if !w.gate.Declared("acme.tools") || w.announced.Load() == before {
		t.Fatal("an upgrade's new key was not declared and announced")
	}
	if err := uninstallConfirmed(t, b, ctx, "acme_backup", true); err != nil {
		t.Fatal(err)
	}
	if w.gate.Declared("acme.backup") || w.gate.Declared("acme.tools") || !w.gate.Declared("acme.vault") {
		t.Fatalf("after uninstall: %+v", w.gate.States())
	}
	// The key is free again.
	install(t, b, w.pkg("acme_off", "1.0.0", declaring("acme.backup")), false)
	if !w.gate.Declared("acme.backup") || b.FeatureOwner("acme_driver", "acme.backup") != "acme_off" || b.FeatureOwner("acme_off", "acme.backup") != "" {
		t.Fatal("a freed key did not pass to its new declarer")
	}
}

// Packages that clash and were installed before installs refused that (or
// placed by hand) still boot: the earlier owner keeps the key, a compiled
// module keeps its own, and each clash is logged.
func TestBootKeepsTheEarlierOwnerOfAClashingKey(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	for _, raw := range [][]byte{
		w.pkg("acme_backup", "1.0.0", declaring("acme.backup", "acme.vault")),
		w.pkg("acme_off", "1.0.0", declaring("acme.backup")),
	} {
		// Straight to the package host, as an install before the check did.
		if _, err := b.pkgs.Install(bytes.NewReader(raw), extpkg.InstallOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	w.static = []featuregate.Feature{{Key: "acme.vault"}}
	b = w.open()
	if got := w.gate.States(); len(got) != 2 || got[0].Key != "acme.backup" || got[1].Key != "acme.vault" {
		t.Fatalf("states = %+v", got)
	}
	// acme.vault is the compiled module's: nothing answers for it, so it
	// is locked, not answered by the package's pushed state.
	if _, err := w.gate.Require("acme.vault", false); err == nil || err.Code() != messages.ErrorFeatureProviderAbsent {
		t.Fatalf("acme.vault = %v", err)
	}
	if got := b.FeatureOwner("acme_off", "acme.backup"); got != "acme_backup" {
		t.Fatalf("owner of acme.backup = %q", got)
	}
	logs := w.logs.String()
	for _, want := range []string{"plugin=acme_off feature=acme.backup owner=acme_backup", "plugin=acme_backup feature=acme.vault owner=ops"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("boot did not log %q:\n%s", want, logs)
		}
	}
}
