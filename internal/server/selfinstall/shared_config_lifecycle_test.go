package selfinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/launchd"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func TestSharedRootUpgradeMigratesConfigurationAndRollbackKeepsItWritable(t *testing.T) {
	for _, failHealth := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback"}[failHealth], func(t *testing.T) {
			root, agent := withSharedRoot(t)
			h := newHarness(t, root)
			h.writeExistingInstall("v1.0.0")
			raw := readFile(t, h.layout.ConfigPath()) + "\n# custom setting\nmetrics_retention: 168h\n"
			writeFile(t, h.layout.ConfigPath(), raw, 0o664)
			h.useSite("v2.0.0", []byte("new binary"))
			if failHealth {
				h.healthErr = errors.New("new version unhealthy")
			}
			report, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
			if (err != nil) != failHealth || report.RolledBack != failHealth {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			path := h.layout.ConfigPath()
			if path != filepath.Join(root, "server-config", "server.yaml") {
				t.Fatalf("upgrade config=%s", path)
			}
			if got := readFile(t, path); got != raw {
				t.Fatalf("upgrade/rollback changed config: %s", got)
			}
			if err := serverconfig.SetAutoCheckUpdates(path, true); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(h.unitFile(), "--config "+path+"\n") {
				t.Fatal("unit still uses the shared-root config path")
			}
			members := readArchive(t, report.Backup)
			if members["server.yaml"] != raw {
				t.Fatalf("backup config member=%q", members["server.yaml"])
			}
			for _, p := range agent {
				if readFile(t, p) != "agent state" {
					t.Fatal("upgrade touched agent")
				}
			}
		})
	}
}

func TestSharedRootUpgradeChecksAndNoOpDoNotMigrate(t *testing.T) {
	for _, check := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-version", true: "check-only"}[check], func(t *testing.T) {
			root, _ := withSharedRoot(t)
			h := newHarness(t, root)
			h.writeExistingInstall("v1.0.0")
			target := "v1.0.0"
			if check {
				target = "v2.0.0"
			}
			h.useSite(target, []byte("new"))
			before := uninstallSnapshot(t, root, h.layout.UnitDir)
			req := h.upgradeReq()
			req.Check = check
			if _, err := h.installer.Upgrade(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			after := uninstallSnapshot(t, root, h.layout.UnitDir)
			// Upgrade already creates/normalizes its single synchronization
			// file on --check and no-op paths; config migration must not run.
			delete(before, h.layout.LockPath())
			delete(after, h.layout.LockPath())
			if !reflect.DeepEqual(before, after) {
				t.Fatal("read-only upgrade migrated or changed installation")
			}
		})
	}
}

func TestSharedRootMigratedUninstallKeepsOrPurgesConfigurationPair(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "purge"}[purge], func(t *testing.T) {
			root, agent := withSharedRoot(t)
			h := newHarness(t, root)
			h.writeExistingInstall("v1.0.0")
			h.installSelf("installed")
			h.mustInstall(h.quiet())
			private := filepath.Join(root, "server-config", "server.yaml")
			alias := filepath.Join(root, "server.yaml")
			if _, err := os.Readlink(alias); err != nil {
				t.Fatal("install did not migrate config", err)
			}
			before := readFile(t, private)
			req := UninstallRequest{Yes: true, Purge: purge, Resources: emptyUninstallResources}
			if _, err := h.installer.Uninstall(t.Context(), req, nil); err != nil {
				t.Fatal(err)
			}
			if purge {
				assertUninstallAbsent(t, private, alias, filepath.Dir(private))
			} else {
				if readFile(t, private) != before || readFile(t, alias) != before {
					t.Fatal("ordinary uninstall changed recovery config")
				}
			}
			for _, p := range agent {
				if readFile(t, p) != "agent state" {
					t.Fatal("uninstall touched agent")
				}
			}
			if _, err := h.installer.Uninstall(t.Context(), req, nil); err != nil {
				t.Fatalf("repeat uninstall: %v", err)
			}
		})
	}
}

func TestSharedRootInstallRejectsUncontrolledConfigPathsBeforeWriting(t *testing.T) {
	for _, bad := range []string{"alias-outside", "absolute-alias", "dangling-alias", "missing-alias", "parent-symlink", "leaf-symlink", "conflicting-private", "world-writable-parent"} {
		t.Run(bad, func(t *testing.T) {
			root, agent := withSharedRoot(t)
			h := newHarness(t, root)
			h.writeExistingInstall("v1.0.0")
			h.installSelf("replacement")
			alias := filepath.Join(root, "server.yaml")
			dir := filepath.Join(root, "server-config")
			private := filepath.Join(dir, "server.yaml")
			switch bad {
			case "alias-outside":
				_ = os.Remove(alias)
				if err := os.Symlink(agent[0], alias); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				if err := os.Symlink(filepath.Dir(agent[0]), dir); err != nil {
					t.Fatal(err)
				}
			case "world-writable-parent":
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0o777); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if bad == "leaf-symlink" {
					if err := os.Symlink(agent[0], private); err != nil {
						t.Fatal(err)
					}
				} else if bad != "dangling-alias" {
					writeFile(t, private, readFile(t, alias), 0o664)
				}
				if bad == "missing-alias" {
					if err := os.Remove(alias); err != nil {
						t.Fatal(err)
					}
				} else if bad != "conflicting-private" {
					_ = os.Remove(alias)
					target := "server-config/server.yaml"
					if bad == "absolute-alias" {
						target = private
					}
					if err := os.Symlink(target, alias); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := uninstallSnapshot(t, root, h.layout.UnitDir)
			if directory := DirOfConfig(private); directory != dir {
				t.Fatalf("uncontrolled private file classified as installation %q", directory)
			}
			if _, err := h.installer.Install(t.Context(), h.quiet()); err == nil {
				t.Fatal("uncontrolled config accepted")
			}
			if after := uninstallSnapshot(t, root, h.layout.UnitDir); !reflect.DeepEqual(before, after) {
				t.Fatal("refused install changed original files")
			}
			if h.run.ran("sudo chown") || h.run.ran("sudo systemctl restart") {
				t.Fatal("refusal occurred after side effects")
			}
		})
	}
}

func TestSharedRootAliasPublicationFailureKeepsOriginalInstallation(t *testing.T) {
	if sharedConfigPermissionChild(t) {
		return
	}
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	h.installSelf("replacement")
	h.installer.deps.Run = &sharedConfigRunner{fakeRunner: h.run, root: root}
	original := readFile(t, h.layout.ConfigPath())
	binary := readFile(t, h.layout.Binary())
	unit := h.unitFile()
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	h.run.fail["sudo sh -c"] = errors.New("alias publication failed")
	if _, err := h.installer.Install(context.Background(), h.quiet()); err == nil {
		t.Fatal("missing publication failure")
	}
	if readFile(t, filepath.Join(root, "server.yaml")) != original || readFile(t, h.layout.Binary()) != binary || h.unitFile() != unit {
		t.Fatal("failed migration changed the prior installation")
	}
	if _, err := os.Lstat(filepath.Join(root, "server-config", "server.yaml")); !os.IsNotExist(err) {
		t.Fatalf("failed migration left conflicting config: %v", err)
	}
}

func TestSharedRootPurgeAliasFailureRestoresConfigAndCanRetry(t *testing.T) {
	if sharedConfigPermissionChild(t) {
		return
	}
	for _, replacement := range []string{"none", "new-config", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			root, agent := withSharedRoot(t)
			h := newHarness(t, root)
			h.writeExistingInstall("v1.0.0")
			h.installSelf("installed")
			h.mustInstall(h.quiet())
			h.installer.deps.Run = &sharedConfigRunner{fakeRunner: h.run, root: root}
			private := filepath.Join(root, "server-config", "server.yaml")
			alias := filepath.Join(root, "server.yaml")
			old := readFile(t, private)
			// The privileged CLI can create this synchronization file in the
			// real root-owned parent; provision that capability before denying
			// the fixture account directory writes.
			writeFile(t, h.layout.LockPath(), "", 0o664)
			if err := os.Chmod(root, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
			command := "sudo rm -rf -- " + alias
			h.run.fail[command] = errors.New("cannot unlink alias")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h.run.on = func(cmd Command) {
				if cmd.String() == command {
					cancel()
					if replacement == "new-config" {
						writeFile(t, private, "new writer's config", 0o664)
					}
					if replacement == "symlink" {
						if err := os.Symlink(agent[0], private); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			req := UninstallRequest{Yes: true, Purge: true, Resources: emptyUninstallResources}
			if _, err := h.installer.Uninstall(ctx, req, nil); err == nil {
				t.Fatal("missing purge failure")
			}
			switch replacement {
			case "none":
				if readFile(t, private) != old || readFile(t, alias) != old {
					t.Fatal("cancelled purge lost discoverable config")
				}
				delete(h.run.fail, command)
				h.run.on = nil
				if _, err := h.installer.Uninstall(t.Context(), req, nil); err != nil {
					t.Fatalf("retry failed: %v", err)
				}
				assertUninstallAbsent(t, alias, private, filepath.Dir(private))
			case "new-config":
				if readFile(t, private) != "new writer's config" {
					t.Fatal("compensation overwrote a newly created config")
				}
			case "symlink":
				if _, err := os.Readlink(private); err != nil {
					t.Fatal("compensation replaced another writer's symlink")
				}
			}
			for _, p := range agent {
				if readFile(t, p) != "agent state" {
					t.Fatal("purge compensation touched agent")
				}
			}
		})
	}
}

func TestSharedRootNewInstallRepairPreservesCommentsAndExplicitRequests(t *testing.T) {
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.installSelf("new binary")
	req := h.quiet()
	req.Password = "new-shared-install-password-2026"
	first := h.mustInstall(req)
	if first.Adopted {
		t.Fatal("new shared installation reported adoption")
	}
	path := first.Layout.ConfigPath()
	old := readFile(t, path) + "\n# operator policy\nusage_report: false\nupdate_channel: nightly\nmetrics_retention: 168h\ntls_cert_file: certs/server.crt\ntls_key_file: certs/server.key\n"
	writeFile(t, path, old, 0o664)
	// Split listeners must be removed when --addr explicitly restores one
	// shared listener; a mapping merge may not keep obsolete child keys.
	cfg := h.config()
	cfg.Listen.Agent = []string{h.freeAddr()}
	cfg.Listen.Gateway = []string{h.freeAddr()}
	if err := serverconfig.SetListen(path, cfg.Listen); err != nil {
		t.Fatal(err)
	}
	req = h.quiet()
	req.LogKeep = 7
	req.LogKeepSet = true
	req.LogMaxSize = "12MB"
	req.LogMaxSizeSet = true
	req.TrustedProxies = "192.0.2.0/24"
	req.TrustedProxiesSet = true
	report := h.mustInstall(req)
	got := h.config()
	if !report.Adopted || got.Listen.Console[0] != req.Addr || len(got.Listen.Agent) != 0 || len(got.Listen.Gateway) != 0 || got.Log.Keep != 7 || got.Log.MaxSize != "12MB" || strings.Join(got.TrustedProxies, ",") != "192.0.2.0/24" {
		t.Fatalf("explicit repair options lost: %+v", got)
	}
	if got.UsageReport == nil || *got.UsageReport || got.UpdateChannel != "nightly" || got.MetricsRetention != "168h" || got.TLSCertFile != "certs/server.crt" || got.TLSKeyFile != "certs/server.key" {
		t.Fatalf("operator config lost: %+v", got)
	}
	if !strings.Contains(readFile(t, path), "# operator policy") {
		t.Fatal("repair lost unmodified comments")
	}
}

func TestSharedRootSupervisorsKeepTheBinaryWorkingDirectory(t *testing.T) {
	for _, mode := range []string{ModeLaunchd, ModeNohup} {
		t.Run(mode, func(t *testing.T) {
			var h *harness
			if mode == ModeLaunchd {
				h = macHarness(t)
				old := sharedApplicationRoot
				sharedApplicationRoot = h.layout.Dir
				t.Cleanup(func() { sharedApplicationRoot = old })
				if err := os.Chmod(h.layout.Dir, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				root, _ := withSharedRoot(t)
				h = newHarness(t, root)
				h.systemd = false
			}
			h.writeExistingInstall("v1.0.0")
			h.installSelf("installed")
			cfg := h.config()
			cfg.TLSCertFile = "certs/server.crt"
			cfg.TLSKeyFile = "certs/server.key"
			h.writeConfig(cfg)
			writeFile(t, filepath.Join(h.layout.Dir, "certs/server.crt"), "fixture cert", 0o600)
			var cwd string
			originalStart := h.installer.deps.StartDetached
			h.installer.deps.StartDetached = func(ctx context.Context, cmd Command) (int, error) { cwd = cmd.Dir; return originalStart(ctx, cmd) }
			req := h.quiet()
			if mode == ModeNohup {
				req.NoSystemd = true
			}
			report := h.mustInstall(req)
			if report.Mode != mode {
				t.Fatalf("mode=%s", report.Mode)
			}
			if mode == ModeLaunchd {
				plist := readFile(t, h.layout.LaunchdPath())
				spec, err := launchd.ReadSpec([]byte(plist))
				if err != nil {
					t.Fatal(err)
				}
				cwd = spec.WorkingDir
			}
			if cwd != h.layout.Dir {
				t.Fatalf("config relocation changed supervisor cwd to %s", cwd)
			}
			if readFile(t, filepath.Join(cwd, h.config().TLSCertFile)) != "fixture cert" {
				t.Fatal("relative config path no longer names the same file")
			}
		})
	}
}

func TestCustomConfigDirectoryIsNotAReservedSharedInstallation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server-config", "server.yaml")
	writeFile(t, path, "listen: {}\n", 0o664)
	if directory := DirOfConfig(path); directory != filepath.Dir(path) {
		t.Fatalf("custom config directory changed to %s", directory)
	}
}

func TestSharedRootPurgeRetainsUnrelatedPrivateDirectoryContents(t *testing.T) {
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	h.installSelf("installed")
	h.mustInstall(h.quiet())
	private := filepath.Join(root, "server-config", "server.yaml")
	alias := filepath.Join(root, "server.yaml")
	notes := filepath.Join(filepath.Dir(private), "operator-notes")
	writeFile(t, notes, "keep unrelated file", 0o600)
	if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Yes: true, Purge: true, Resources: emptyUninstallResources}, nil); err != nil {
		t.Fatal(err)
	}
	assertUninstallAbsent(t, private, alias)
	if readFile(t, notes) != "keep unrelated file" {
		t.Fatal("purge removed unrelated config-directory contents")
	}
}

func TestSharedRootPublishedAliasCleanupFailureNeverDeletesConfiguration(t *testing.T) {
	if sharedConfigPermissionChild(t) {
		return
	}
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	h.installSelf("replacement")
	original := readFile(t, h.layout.ConfigPath())
	h.installer.deps.Run = &sharedConfigRunner{fakeRunner: h.run, root: root}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	var cleanupCommand string
	h.run.on = func(cmd Command) {
		if cmd.Path == "sh" && len(cmd.Args) > 3 && cmd.Args[2] == "gpuworks-config-alias" {
			cleanupCommand = "sudo rm -rf -- " + cmd.Args[3]
			h.run.fail[cleanupCommand] = errors.New("post-publication stage cleanup failed")
		}
	}
	if _, err := h.installer.Install(t.Context(), h.quiet()); err == nil {
		t.Fatal("missing post-publication cleanup error")
	}
	alias := filepath.Join(root, "server.yaml")
	private := filepath.Join(root, "server-config", "server.yaml")
	if target, err := os.Readlink(alias); err != nil || target != "server-config/server.yaml" {
		t.Fatalf("alias was not published: %q %v", target, err)
	}
	if readFile(t, alias) != original || readFile(t, private) != original {
		t.Fatal("cleanup failure lost the published configuration")
	}
	if readFile(t, h.layout.Binary()) != "old binary" {
		t.Fatal("failed migration replaced binary")
	}
	if cleanupCommand == "" {
		t.Fatal("fixture never reached alias publication")
	}
	delete(h.run.fail, cleanupCommand)
	h.run.on = nil
	report := h.mustInstall(h.quiet())
	if report.Layout.ConfigPath() != private {
		t.Fatal("retry could not adopt the already-published config")
	}
}

func TestSharedRootUpgradeRetriesPrivateServiceConfigurationAfterSetupFailure(t *testing.T) {
	for _, failure := range []string{"unit-write", "daemon-reload"} {
		t.Run(failure, func(t *testing.T) {
			root, _ := withSharedRoot(t)
			h := newHarness(t, root)
			h.writeExistingInstall("v1.0.0")
			h.useSite("v2.0.0", []byte("new binary"))
			private := filepath.Join(root, "server-config", "server.yaml")
			// systemd retains its old loaded definition until a successful reload.
			// A private on-disk file alone must not make this fixture start privately.
			loadedConfig := filepath.Join(root, "server.yaml")
			var starts []string
			h.run.on = func(cmd Command) {
				if cmd.String() == "sudo systemctl daemon-reload" && h.run.failure(cmd) == nil {
					if strings.Contains(readFile(t, h.layout.UnitPath()), "--config "+private+"\n") {
						loadedConfig = private
					}
				}
				if cmd.String() == "sudo systemctl restart gpuworks" {
					starts = append(starts, loadedConfig)
				}
			}
			failCommand := "sudo systemctl daemon-reload"
			if failure == "unit-write" {
				failCommand = "sudo tee " + h.layout.UnitPath()
			}
			h.run.fail[failCommand] = errors.New("initial service config failed")
			if _, err := h.installer.Upgrade(t.Context(), h.upgradeReq()); err == nil {
				t.Fatal("missing service setup failure")
			}
			if h.layout.ConfigPath() != private || readFile(t, h.layout.Binary()) != "old binary" {
				t.Fatal("failure did not preserve published config and old binary")
			}
			delete(h.run.fail, failCommand)
			h.installer = New(h.layout, h.deps, h.io)
			if _, err := h.installer.Upgrade(t.Context(), h.upgradeReq()); err != nil {
				t.Fatal(err)
			}
			if len(starts) != 1 || starts[0] != private {
				t.Fatalf("retry started cached shared-root config: %v", starts)
			}
		})
	}
}
