package serverconfig_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func TestOutboundPrivateHostsDefaultsAndExplicitRestriction(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        []string
	}{
		{"omitted", "", []string{"mirror.internal", "0.0.0.0/0", "::/0"}},
		{"false", "outbound_restrict_private_hosts: false\n", []string{"mirror.internal", "0.0.0.0/0", "::/0"}},
		{"true", "outbound_restrict_private_hosts: true\n", []string{"mirror.internal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			writeRawOutboundConfig(t, path, tc.field+"outbound_allow_private_hosts: [mirror.internal]\n")
			read := serverconfig.WatchOutboundPrivateHosts(path, true, []string{"startup.internal"}, nil)
			assertOutboundHosts(t, read, tc.want...)
		})
	}
}

func TestOutboundPrivateHostsRestrictionHotChangesBothWays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	read := serverconfig.WatchOutboundPrivateHosts(path, false, []string{"startup.internal"}, nil)
	writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: true\noutbound_allow_private_hosts: [mirror.internal]\n")
	assertOutboundHosts(t, read, "mirror.internal")
	writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: false\noutbound_allow_private_hosts: [mirror.internal]\n")
	assertOutboundHosts(t, read, "mirror.internal", "0.0.0.0/0", "::/0")
	writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: true\noutbound_allow_private_hosts: []\n")
	assertOutboundHosts(t, read)
	writeRawOutboundConfig(t, path, "{}\n")
	assertOutboundHosts(t, read, "0.0.0.0/0", "::/0")
}

func TestOutboundPrivateHostsWithoutFileDefaultsOpenAndCopiesInputs(t *testing.T) {
	initial := make([]string, 1, 4)
	initial[0] = "mirror.internal"
	read := serverconfig.WatchOutboundPrivateHosts("", false, initial, nil)
	initial[0] = "caller-mutated.internal"
	assertOutboundHosts(t, read, "mirror.internal", "0.0.0.0/0", "::/0")
	got := read()
	got[0] = "caller-return.internal"
	got[1] = "changed"
	assertOutboundHosts(t, read, "mirror.internal", "0.0.0.0/0", "::/0")
	strict := serverconfig.WatchOutboundPrivateHosts("", true, nil, nil)
	assertOutboundHosts(t, strict)
}

func TestOutboundPrivateHostsOpenModeFailuresRevokeAndRecover(t *testing.T) {
	for _, scenario := range []string{"missing", "malformed", "unreadable", "oversized", "invalid-bool"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: false\n")
			read := serverconfig.WatchOutboundPrivateHosts(path, false, []string{"startup.internal"}, nil)
			assertOutboundHosts(t, read, "0.0.0.0/0", "::/0")
			switch scenario {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				writeRawOutboundConfig(t, path, "outbound_allow_private_hosts: [unterminated\n")
			case "invalid-bool":
				writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: [false]\n")
			case "unreadable":
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0600) })
				if _, err := os.ReadFile(path); err == nil {
					t.Skip("current identity can read mode-000 files")
				}
			case "oversized":
				writeRawOutboundConfig(t, path, strings.Repeat("# padding\n", 220000))
			}
			assertOutboundHosts(t, read)
			if scenario == "unreadable" {
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: false\n")
			assertOutboundHosts(t, read, "0.0.0.0/0", "::/0")
		})
	}
}

func TestOutboundPrivateHostsRestrictionRoundTripsAndRejectsWrongTypes(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		name := "open"
		if restricted {
			name = "restricted"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			expected := serverconfig.File{OutboundRestrictPrivateHosts: restricted, OutboundAllowPrivateHosts: []string{"mirror.internal", "10.20.0.0/16"}}
			if err := serverconfig.Write(path, expected, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := serverconfig.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("round trip = %+v, want %+v", got, expected)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if restricted && !strings.Contains(string(raw), "outbound_restrict_private_hosts: true") {
				t.Fatalf("strict mode not persisted as YAML bool: %s", raw)
			}
		})
	}
	for _, invalid := range []string{"[true]", "{enabled: true}", "not-a-bool"} {
		t.Run(invalid, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: "+invalid+"\n")
			if _, err := serverconfig.Load(path); err == nil {
				t.Fatal("invalid restriction type accepted")
			}
		})
	}
}
