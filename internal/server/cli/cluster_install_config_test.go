package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const clusterConfigFixture = `version: 1
defaults:
  dir: /home/ubuntu/gpuworks-cluster
  ssh:
    user: ubuntu
    key: keys/default.key
    port: 2222
ports:
  http: 18800
  enrollment: 19443
  database: 19001
nodes:
  - name: control-1
    address: 192.0.2.11
    ssh:
      target: node-one
  - name: control-2
    address: 192.0.2.12
    dir: /home/admin/gpuworks-cluster
    ssh:
      target: node-two
      user: admin
      key: keys/admin.key
      port: 2202
  - name: control-3
    address: 192.0.2.13
`

func writeClusterConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClusterConfigInheritsNodeSettingsAndKeepsAddressesSeparate(t *testing.T) {
	config := writeClusterConfig(t, clusterConfigFixture)
	password := filepath.Join(filepath.Dir(config), "cluster-admin.password")
	if err := os.WriteFile(password, []byte("private-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	o, err := parseClusterInstall([]string{"--binary", "/tmp/gpuworks-ha", "--config", config, "--password-file", password}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.Hosts, []string{"192.0.2.11", "192.0.2.12", "192.0.2.13"}) || len(o.Nodes) != 3 {
		t.Fatalf("cluster addresses were replaced by SSH aliases: %#v", o)
	}
	want := []clusterInstallNode{
		{Name: "control-1", Address: "192.0.2.11", Dir: "/home/ubuntu/gpuworks-cluster", SSH: clusterInstallSSH{Target: "node-one", User: "ubuntu", Key: filepath.Join(filepath.Dir(config), "keys/default.key"), Port: 2222}},
		{Name: "control-2", Address: "192.0.2.12", Dir: "/home/admin/gpuworks-cluster", SSH: clusterInstallSSH{Target: "node-two", User: "admin", Key: filepath.Join(filepath.Dir(config), "keys/admin.key"), Port: 2202}},
		{Name: "control-3", Address: "192.0.2.13", Dir: "/home/ubuntu/gpuworks-cluster", SSH: clusterInstallSSH{Target: "192.0.2.13", User: "ubuntu", Key: filepath.Join(filepath.Dir(config), "keys/default.key"), Port: 2222}},
	}
	for _, node := range want {
		if got := o.node(node.Address); !reflect.DeepEqual(got, node) {
			t.Errorf("node %s = %#v, want %#v", node.Address, got, node)
		}
	}
	if o.Password != "private-password" || o.HTTP != 18800 || o.Enrollment != 19443 || o.Database != 19001 {
		t.Fatalf("wrong separate password or global ports: %#v", o)
	}
	if !filepath.IsAbs(o.Binary) {
		t.Fatalf("default binary is not absolute: %q", o.Binary)
	}
}

func TestClusterConfigExplicitPortsOverrideOnlyTheirYAMLSettings(t *testing.T) {
	config := writeClusterConfig(t, clusterConfigFixture)
	o, err := parseClusterOptions([]string{"--binary", "/tmp/gpuworks-ha", "--config", config, "--http-port", "28000", "--database-port", "29000"}, io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	if o.HTTP != 28000 || o.Enrollment != 19443 || o.Database != 29000 {
		t.Fatalf("flag precedence lost YAML ports: %#v", o)
	}
	for _, args := range [][]string{{"--http-port", "19001"}, {"--database-port", "80"}, {"--enrollment-port", "65536"}} {
		if _, err := parseClusterOptions(append([]string{"--binary", "/tmp/gpuworks-ha", "--config", config}, args...), io.Discard, true); err == nil {
			t.Fatalf("unsafe overridden ports accepted: %v", args)
		}
	}
}

func TestClusterConfigRejectsConflictingShortcutFlags(t *testing.T) {
	config := writeClusterConfig(t, clusterConfigFixture)
	for _, args := range [][]string{
		{"--hosts", "192.0.2.21,192.0.2.22,192.0.2.23"},
		{"--ssh-user", "operator"},
		{"--ssh-key", "/private/key"},
		{"--dir", "/home/operator/new-cluster"},
		{"--ssh-user", ""},
	} {
		t.Run(args[0], func(t *testing.T) {
			if _, err := parseClusterOptions(append([]string{"--binary", "/tmp/gpuworks-ha", "--config", config}, args...), io.Discard, true); err == nil || !strings.Contains(err.Error(), args[0]) {
				t.Fatalf("conflict %v must identify the flag: %v", args, err)
			}
		})
	}
}

func TestClusterConfigRejectsInvalidAndUnsafeYAML(t *testing.T) {
	cases := map[string]string{
		"missing version":         strings.TrimPrefix(clusterConfigFixture, "version: 1\n"),
		"unsupported version":     strings.Replace(clusterConfigFixture, "version: 1", "version: 2", 1),
		"unknown root":            clusterConfigFixture + "unexpected: true\n",
		"unknown ssh":             strings.Replace(clusterConfigFixture, "      target: node-one", "      target: node-one\n      options: -oStrictHostKeyChecking=no", 1),
		"duplicate key":           clusterConfigFixture + "version: 1\n",
		"multiple documents":      clusterConfigFixture + "---\nversion: 1\n",
		"malformed yaml":          "version: [\n",
		"password root":           clusterConfigFixture + "password: do-not-store-this\n",
		"password default":        strings.Replace(clusterConfigFixture, "  dir: /home/ubuntu/gpuworks-cluster", "  password: do-not-store-this\n  dir: /home/ubuntu/gpuworks-cluster", 1),
		"password node":           strings.Replace(clusterConfigFixture, "    address: 192.0.2.11", "    password: do-not-store-this\n    address: 192.0.2.11", 1),
		"two nodes":               strings.TrimSuffix(clusterConfigFixture, "  - name: control-3\n    address: 192.0.2.13\n"),
		"four nodes":              clusterConfigFixture + "  - name: control-4\n    address: 192.0.2.14\n",
		"duplicate address":       strings.Replace(clusterConfigFixture, "192.0.2.13", "192.0.2.11", 1),
		"duplicate name":          strings.Replace(clusterConfigFixture, "name: control-3", "name: control-1", 1),
		"ipv6 address":            strings.Replace(clusterConfigFixture, "192.0.2.13", "2001:db8::13", 1),
		"hostname address":        strings.Replace(clusterConfigFixture, "192.0.2.13", "node-three", 1),
		"loopback address":        strings.Replace(clusterConfigFixture, "192.0.2.13", "127.0.0.1", 1),
		"multicast address":       strings.Replace(clusterConfigFixture, "192.0.2.13", "224.0.0.1", 1),
		"unspecified address":     strings.Replace(clusterConfigFixture, "192.0.2.13", "0.0.0.0", 1),
		"unsafe target":           strings.Replace(clusterConfigFixture, "target: node-one", "target: '-oProxyCommand=touch /tmp/bad'", 1),
		"target with user":        strings.Replace(clusterConfigFixture, "target: node-one", "target: ubuntu@node-one", 1),
		"unsafe user":             strings.Replace(clusterConfigFixture, "user: admin", "user: 'admin; touch /tmp/bad'", 1),
		"unsafe node name":        strings.Replace(clusterConfigFixture, "name: control-1", "name: 'control; echo bad'", 1),
		"unclean directory":       strings.Replace(clusterConfigFixture, "/home/admin/gpuworks-cluster", "/home/admin/../gpuworks-cluster", 1),
		"root directory":          strings.Replace(clusterConfigFixture, "/home/admin/gpuworks-cluster", "/", 1),
		"relative directory":      strings.Replace(clusterConfigFixture, "/home/admin/gpuworks-cluster", "gpuworks-cluster", 1),
		"negative ssh port":       strings.Replace(clusterConfigFixture, "port: 2202", "port: -1", 1),
		"overflow ssh port":       strings.Replace(clusterConfigFixture, "port: 2202", "port: 65536", 1),
		"invalid key newline":     strings.Replace(clusterConfigFixture, "key: keys/admin.key", `key: "key\nnext"`, 1),
		"default target":          strings.Replace(clusterConfigFixture, "    user: ubuntu", "    target: same-host\n    user: ubuntu", 1),
		"duplicate cluster ports": strings.Replace(clusterConfigFixture, "database: 19001", "database: 18800", 1),
		"privileged cluster port": strings.Replace(clusterConfigFixture, "http: 18800", "http: 80", 1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			config := writeClusterConfig(t, content)
			if _, err := parseClusterOptions([]string{"--binary", "/tmp/gpuworks-ha", "--config", config}, io.Discard, true); err == nil {
				t.Fatal("invalid config accepted")
			} else if strings.Contains(err.Error(), "do-not-store-this") {
				t.Fatal("error echoed secret from invalid config")
			}
		})
	}
}

func TestClusterConfigRequiresBoundedSingleRegularFile(t *testing.T) {
	for name, path := range map[string]string{"directory": t.TempDir(), "missing": filepath.Join(t.TempDir(), "missing.yaml"), "oversize": writeClusterConfig(t, clusterConfigFixture+strings.Repeat("# filler\n", 8192))} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseClusterOptions([]string{"--binary", "/tmp/gpuworks-ha", "--config", path}, io.Discard, true); err == nil {
				t.Fatal("invalid config file accepted")
			}
		})
	}
}

func TestClusterCheckDoesNotRequireOrReadAdminPassword(t *testing.T) {
	config := writeClusterConfig(t, clusterConfigFixture)
	o, err := parseClusterOptions([]string{"--binary", "/tmp/gpuworks-ha", "--config", config}, io.Discard, true)
	if err != nil || o.Password != "" {
		t.Fatalf("preflight must parse without admin credentials: %#v, %v", o, err)
	}
	if _, err := parseClusterInstall([]string{"--binary", "/tmp/gpuworks-ha", "--config", config}, io.Discard); err == nil || !strings.Contains(err.Error(), "password-file") {
		t.Fatalf("installation accepted missing admin credentials: %v", err)
	}
	if _, err := parseClusterOptions([]string{"--binary", "/tmp/gpuworks-ha", "--config", config, "--password-file", filepath.Join(t.TempDir(), "does-not-exist")}, io.Discard, true); err == nil || !strings.Contains(err.Error(), "password-file") {
		t.Fatalf("preflight accepted unnecessary password-file: %v", err)
	}
}

func TestClusterConfigLeavesUnspecifiedSSHSettingsToOpenSSH(t *testing.T) {
	config := writeClusterConfig(t, `version: 1
defaults:
  dir: /srv/gpuworks-cluster
nodes:
  - address: 192.0.2.11
    ssh:
      target: node-one
  - address: 192.0.2.12
  - address: 192.0.2.13
`)
	o, err := parseClusterOptions([]string{"--binary", "/tmp/gpuworks-ha", "--config", config}, io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, node := range o.Nodes {
		if node.SSH.User != "" || node.SSH.Key != "" || node.SSH.Port != 0 || node.Name == "" {
			t.Fatalf("node %d overrides OpenSSH or lacks a diagnostic name: %#v", i, node)
		}
	}
	legacy := []string{"--binary", "/tmp/gpuworks-ha", "--hosts", "192.0.2.11,192.0.2.12,192.0.2.13", "--dir", "/srv/gpuworks-cluster"}
	if _, err := parseClusterOptions(legacy, io.Discard, true); err == nil || !strings.Contains(err.Error(), "ssh-user") {
		t.Fatalf("legacy syntax silently changed its required account: %v", err)
	}
}

func TestClusterSSHArgsKeepEachSettingInOneArgument(t *testing.T) {
	key := "/private/keys/a key;$(touch sentinel)"
	node := clusterInstallNode{SSH: clusterInstallSSH{Target: "node-two", User: "admin", Key: key, Port: 2202}}
	want := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "-o", "StrictHostKeyChecking=yes", "-i", key, "-l", "admin", "-p", "2202", "node-two"}
	if got := clusterInstallSSHArgs(node); !reflect.DeepEqual(got, want) {
		t.Fatalf("SSH settings are not independent argv entries: %#v", got)
	}
	args := clusterInstallSSHArgs(clusterInstallNode{SSH: clusterInstallSSH{Target: "node-one"}})
	if !reflect.DeepEqual(args, append(append([]string{}, want[:6]...), "node-one")) {
		t.Fatalf("unset settings unexpectedly override OpenSSH: %#v", args)
	}
	if args[len(args)-1] != "node-one" || strings.Contains(strings.Join(args, " "), "-p 22") {
		t.Fatalf("alias or inherited port lost: %#v", args)
	}
}

func TestClusterSSHArgsRespectRealOpenSSHConfigAndExplicitOverrides(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client unavailable")
	}
	config := filepath.Join(t.TempDir(), "ssh-config")
	explicitKey := filepath.Join(filepath.Dir(config), "node-key")
	if err := os.WriteFile(explicitKey, []byte("key fixture; ssh -G does not authenticate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("Host node-two\n  HostName 192.0.2.12\n  User operator\n  Port 2222\n  IdentityFile /private/from-ssh-config\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		ssh             clusterInstallSSH
		user, port, key string
	}{
		{"inherited", clusterInstallSSH{Target: "node-two"}, "operator", "2222", "/private/from-ssh-config"},
		{"explicit", clusterInstallSSH{Target: "node-two", User: "admin", Port: 2202, Key: explicitKey}, "admin", "2202", explicitKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-G", "-F", config}, clusterInstallSSHArgs(clusterInstallNode{SSH: tc.ssh})...)
			raw, err := exec.CommandContext(context.Background(), ssh, args...).Output()
			if err != nil {
				t.Fatal(err)
			}
			lines := "\n" + string(raw)
			for _, field := range []string{"user " + tc.user, "hostname 192.0.2.12", "port " + tc.port, "identityfile " + tc.key, "batchmode yes", "stricthostkeychecking true"} {
				if !strings.Contains(lines, "\n"+field+"\n") {
					t.Errorf("ssh -G did not resolve %q: %s", field, raw)
				}
			}
		})
	}
}

func TestClusterConfigInstallerUsesEachNodesDirectoryAndAccount(t *testing.T) {
	for _, failure := range []string{"", "copy-second", "join-second"} {
		t.Run(failure, func(t *testing.T) {
			config := writeClusterConfig(t, clusterConfigFixture)
			o, err := parseClusterOptions([]string{"--binary", "/tmp/gpuworks-ha", "--config", config}, io.Discard, true)
			if err != nil {
				t.Fatal(err)
			}
			o.Binary = offlineInstallOptions(t).Binary
			o.Password = "separate-private-password"
			var events []string
			sentinel := errors.New("injected node failure")
			shell := func(ctx context.Context, host, command string, input io.Reader) error {
				node := o.node(host)
				phase := "preflight"
				if input != nil {
					phase = "copy"
					raw, err := io.ReadAll(input)
					if err != nil || len(raw) != 32 || !bytes.Equal(raw[:4], []byte{0x7f, 'E', 'L', 'F'}) {
						t.Fatal("per-node copy lost the local executable")
					}
				}
				if !strings.Contains(command, clusterInstallQuote(node.Dir)) || strings.Contains(command, o.Password) {
					t.Fatalf("%s did not use its own directory or leaked credentials: %s", host, command)
				}
				if phase == "preflight" && !strings.Contains(command, clusterInstallUnit(node.Dir)) {
					t.Fatalf("%s preflight used the wrong systemd unit: %s", host, command)
				}
				events = append(events, phase+":"+host)
				if failure == "copy-second" && phase == "copy" && host == o.Hosts[1] {
					return sentinel
				}
				return nil
			}
			remote := func(ctx context.Context, host string, input io.Reader) ([]byte, error) {
				var req clusterInstallNodeRequest
				if err := json.NewDecoder(input).Decode(&req); err != nil {
					t.Fatal(err)
				}
				node := o.node(host)
				if req.Host != node.Address || req.Dir != node.Dir || req.User != node.SSH.User || !reflect.DeepEqual(req.Hosts, o.Hosts) || req.HTTP != 18800 || req.Enrollment != 19443 || req.Database != 19001 {
					t.Fatalf("%s %s lost its settings: %#v", host, req.Action, req)
				}
				events = append(events, req.Action+":"+host)
				if req.Action == "capabilities" {
					if req.Password != "" {
						t.Fatal("capabilities received the administrator password")
					}
					return []byte(`{"native_dqlite":true,"os":"linux"}`), nil
				}
				if req.Password != o.Password {
					t.Fatal("installation did not retain its separate password")
				}
				switch req.Action {
				case "prepare", "serve-joined", "service":
					return []byte(`{"completed":true}`), nil
				case "seed":
					if host != o.Hosts[0] {
						t.Fatal("initialized a different seed")
					}
					return []byte(`{"cluster_id":"test-cluster","ca_fingerprint":"test-fingerprint"}`), nil
				case "invite":
					var target struct{ Host string }
					if err := json.Unmarshal(req.Invitation, &target); err != nil || host != o.Hosts[0] || (target.Host != o.Hosts[1] && target.Host != o.Hosts[2]) {
						t.Fatal("invitation did not use the seed settings and target cluster address")
					}
					return []byte(`{"cluster_id":"test-cluster","token":"private-invitation"}`), nil
				case "join":
					if !bytes.Contains(req.Invitation, []byte(`"token":"private-invitation"`)) {
						t.Fatal("node invitation missing")
					}
					if failure == "join-second" && host == o.Hosts[1] {
						return nil, sentinel
					}
					return []byte(`{"completed":true}`), nil
				case "bootstrap":
					return []byte(`{"bootstrap_ready":true}`), nil
				case "status":
					return []byte(`{"status":"Status report\nOverall: ready (business reads verified)\n"}`), nil
				case "business", "read-setting":
					var setting struct{ Key, Value string }
					if err := json.Unmarshal(req.Invitation, &setting); err != nil || !strings.HasPrefix(setting.Key, "cluster_install_") || setting.Value == "" {
						t.Fatal("invalid business verification request")
					}
					return []byte(`{"completed":true}`), nil
				default:
					t.Fatalf("unknown helper action %q", req.Action)
					return nil, errors.New("unsupported fixture action")
				}
			}
			var output bytes.Buffer
			err = installCluster(t.Context(), o, &output, remote, shell)
			if failure != "" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("per-node failure was not propagated: %v", err)
				}
				for _, event := range events {
					if strings.HasPrefix(event, "service:") || strings.HasPrefix(event, "business:") || (failure == "copy-second" && strings.HasPrefix(event, "prepare:")) {
						t.Fatalf("installer continued past %s: %v", failure, events)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, node := range o.Nodes {
				if !strings.Contains(output.String(), node.Dir) || !strings.Contains(output.String(), clusterInstallUnit(node.Dir)) {
					t.Fatalf("successful install did not identify %s's directory and service: %s", node.Name, output.String())
				}
				for _, action := range []string{"prepare", "bootstrap", "business", "read-setting"} {
					found := false
					for _, event := range events {
						found = found || event == action+":"+node.Address
					}
					if !found {
						t.Fatalf("node %s missed %s: %v", node.Name, action, events)
					}
				}
			}
			if strings.Contains(output.String(), o.Password) || strings.Contains(output.String(), "private-invitation") {
				t.Fatal("public install result exposed credentials")
			}
		})
	}
}

func TestClusterSSHResolutionUsesPerHostAccountsAndRejectsBrokenConfig(t *testing.T) {
	for _, failure := range []string{"", "missing-user", "unsafe-user", "ssh-error"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			first := "printf 'host node-one\\nuser ubuntu\\nhostname 192.0.2.11\\n'"
			switch failure {
			case "missing-user":
				first = "printf 'host node-one\\nhostname 192.0.2.11\\n'"
			case "unsafe-user":
				first = "printf 'user -unsafe\\n'"
			case "ssh-error":
				first = "exit 255"
			}
			// A local OpenSSH config query never connects. This fixture requires
			// -G and returns the same line-oriented User/HostName fields, including
			// missing/invalid fields and OpenSSH's configuration failure exit.
			script := "#!/bin/sh\nset -eu\ntest \"$1\" = -G\nfor target do :; done\ncase \"$target\" in\nnode-one) " + first + ";;\nnode-two) printf 'host node-two\\nuser admin\\nhostname 192.0.2.12\\n';;\nnode-three) printf 'host node-three\\nuser operator\\nhostname 192.0.2.13\\n';;\n*) exit 255;;\nesac\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			o := clusterInstallOptions{Hosts: []string{"192.0.2.11", "192.0.2.12", "192.0.2.13"}, Nodes: []clusterInstallNode{
				{Name: "control-1", Address: "192.0.2.11", SSH: clusterInstallSSH{Target: "node-one"}},
				{Name: "control-2", Address: "192.0.2.12", SSH: clusterInstallSSH{Target: "node-two"}},
				{Name: "control-3", Address: "192.0.2.13", SSH: clusterInstallSSH{Target: "node-three"}},
			}}
			err := resolveClusterInstallSSH(t.Context(), &o)
			if failure == "" {
				if err != nil || o.Nodes[0].SSH.User != "ubuntu" {
					t.Fatalf("OpenSSH account was not resolved: %#v, %v", o.Nodes, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "control-1") || o.Nodes[0].SSH.User != "" {
				t.Fatalf("broken OpenSSH config accepted or reported without node: %#v, %v", o.Nodes, err)
			}
			if o.Nodes[1].SSH.User != "admin" || o.Nodes[2].SSH.User != "operator" {
				t.Fatalf("SSH resolution lost per-host accounts or stopped at one failure: %#v", o.Nodes)
			}
		})
	}
}

func TestClusterSSHResolutionRejectsMissingOrNonregularExplicitKeys(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			key := filepath.Join(dir, "private-key")
			switch kind {
			case "directory":
				if err := os.Mkdir(key, 0700); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if os.Geteuid() == 0 {
					t.Skip("root can read mode-000 files")
				}
				if err := os.WriteFile(key, []byte("key fixture"), 0000); err != nil {
					t.Fatal(err)
				}
			}
			called := filepath.Join(dir, "ssh-called")
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf called > "+clusterInstallQuote(called)+"\nexit 255\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			o := clusterInstallOptions{Nodes: []clusterInstallNode{
				{Name: "control-1", SSH: clusterInstallSSH{Target: "node-one", Key: key}},
				{Name: "control-2", SSH: clusterInstallSSH{Target: "node-two", Key: key}},
			}}
			err := resolveClusterInstallSSH(t.Context(), &o)
			if err == nil || !strings.Contains(err.Error(), "control-1") || !strings.Contains(err.Error(), "control-2") || !strings.Contains(err.Error(), "SSH key") {
				t.Fatalf("invalid explicit keys did not produce per-node failures: %v", err)
			}
			if _, err := os.Stat(called); !os.IsNotExist(err) {
				t.Fatal("invalid explicit key fell back to OpenSSH config")
			}
		})
	}
}

func TestClusterSSHResolutionUsesRealOpenSSHForAliasesAndLegacyHosts(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client unavailable")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "ssh-config")
	content := "Host node-one\n  HostName 192.0.2.11\n  User ubuntu\nHost node-two\n  HostName 192.0.2.12\n  User admin\nHost node-three\n  HostName 192.0.2.13\n  User operator\nHost 192.0.2.*\n  User config-account\n"
	if err := os.WriteFile(config, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nexec " + clusterInstallQuote(ssh) + " -F " + clusterInstallQuote(config) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	hosts := []string{"192.0.2.11", "192.0.2.12", "192.0.2.13"}
	o := clusterInstallOptions{Hosts: hosts, Nodes: []clusterInstallNode{
		{Name: "control-1", Address: hosts[0], SSH: clusterInstallSSH{Target: "node-one"}},
		{Name: "control-2", Address: hosts[1], SSH: clusterInstallSSH{Target: "node-two"}},
		{Name: "control-3", Address: hosts[2], SSH: clusterInstallSSH{Target: "node-three"}},
	}}
	if err := resolveClusterInstallSSH(t.Context(), &o); err != nil {
		t.Fatal(err)
	}
	for i, user := range []string{"ubuntu", "admin", "operator"} {
		if o.Nodes[i].SSH.User != user {
			t.Fatalf("production resolver lost account for alias %s: %#v", o.Nodes[i].SSH.Target, o.Nodes)
		}
	}
	legacy := clusterInstallOptions{Hosts: hosts, User: "legacy-account", Dir: "/home/legacy-account/gpuworks-cluster"}
	if err := resolveClusterInstallSSH(t.Context(), &legacy); err != nil {
		t.Fatal(err)
	}
	if len(legacy.Nodes) != 3 {
		t.Fatalf("legacy settings did not become three node settings: %#v", legacy)
	}
	for i, node := range legacy.Nodes {
		if node.Address != hosts[i] || node.SSH.Target != hosts[i] || node.SSH.User != "legacy-account" || node.Dir != legacy.Dir {
			t.Fatalf("legacy settings were overridden by config-account: %#v", legacy.Nodes)
		}
	}
}

func TestClusterConfigRejectsDerivedSessionPortOverflowAndCollisions(t *testing.T) {
	config := writeClusterConfig(t, clusterConfigFixture)
	legacy := []string{"--binary", "/tmp/gpuworks-ha", "--hosts", "192.0.2.11,192.0.2.12,192.0.2.13", "--ssh-user", "ubuntu", "--dir", "/home/ubuntu/gpuworks-cluster"}
	for _, base := range [][]string{{"--binary", "/tmp/gpuworks-ha", "--config", config}, legacy} {
		for _, ports := range [][]string{
			{"--database-port", "65535"},
			{"--database-port", "9001", "--http-port", "9002"},
			{"--database-port", "9001", "--enrollment-port", "9002"},
		} {
			if _, err := parseClusterOptions(append(append([]string{}, base...), ports...), io.Discard, true); err == nil {
				t.Fatalf("derived session port overflow or conflict accepted: %v %v", base, ports)
			}
		}
		if _, err := parseClusterOptions(append(append([]string{}, base...), "--database-port", "65534"), io.Discard, true); err != nil {
			t.Fatalf("last usable database port (session 65535) rejected: %v", err)
		}
	}
	for _, content := range []string{
		strings.Replace(clusterConfigFixture, "database: 19001", "database: 65535", 1),
		strings.Replace(clusterConfigFixture, "http: 18800", "http: 19002", 1),
		strings.Replace(clusterConfigFixture, "enrollment: 19443", "enrollment: 19002", 1),
	} {
		if _, err := parseClusterOptions([]string{"--binary", "/tmp/gpuworks-ha", "--config", writeClusterConfig(t, content)}, io.Discard, true); err == nil {
			t.Fatal("YAML derived session port overflow or conflict accepted")
		}
	}
}
