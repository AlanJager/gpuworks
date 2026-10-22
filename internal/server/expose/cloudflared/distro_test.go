package cloudflared

import (
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	raw := []byte(`# comment
NAME="Ubuntu"
VERSION_ID="22.04"
ID=ubuntu
ID_LIKE=debian
PRETTY_NAME="Ubuntu 22.04.5 LTS"
HOME_URL="https://www.ubuntu.com/"
`)
	got := ParseOSRelease(raw)
	if got.ID != "ubuntu" {
		t.Errorf("ID = %q, want ubuntu", got.ID)
	}
	if len(got.IDLike) != 1 || got.IDLike[0] != "debian" {
		t.Errorf("IDLike = %v, want [debian]", got.IDLike)
	}
	if got.Pretty != "Ubuntu 22.04.5 LTS" {
		t.Errorf("Pretty = %q", got.Pretty)
	}
}

func TestParseOSReleaseMultipleLikesAndQuotes(t *testing.T) {
	got := ParseOSRelease([]byte("ID='rocky'\nID_LIKE=\"rhel centos fedora\"\n"))
	if got.ID != "rocky" {
		t.Errorf("ID = %q, want rocky (quotes stripped)", got.ID)
	}
	if len(got.IDLike) != 3 {
		t.Fatalf("IDLike = %v, want three entries", got.IDLike)
	}
}

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		name     string
		explicit Format
		raw      string
		cmds     []string
		want     Format
		wantErr  bool
	}{
		{name: "ubuntu", raw: "ID=ubuntu\nID_LIKE=debian\n", want: FormatDeb},
		{name: "debian", raw: "ID=debian\n", want: FormatDeb},
		{name: "rhel", raw: "ID=\"rhel\"\nID_LIKE=\"fedora\"\n", want: FormatRPM},
		{name: "centos", raw: "ID=centos\nID_LIKE=\"rhel fedora\"\n", want: FormatRPM},
		{name: "rocky", raw: "ID=rocky\nID_LIKE=\"rhel centos fedora\"\n", want: FormatRPM},
		{name: "openEuler", raw: "ID=openEuler\n", want: FormatRPM},
		// Helix is the distribution on dev1 and gpu3 and is an RPM family
		// system. Its real /etc/os-release has not been read from a host yet,
		// so both shapes — declaring the family, or only itself — are covered:
		// the ID entry makes the choice right either way.
		{name: "helix with rhel family", raw: "ID=helix\nID_LIKE=\"rhel centos\"\n", want: FormatRPM},
		{name: "helix alone", raw: "ID=helix\n", want: FormatRPM},
		{name: "explicit deb wins over rpm distro", explicit: FormatDeb, raw: "ID=rhel\n", want: FormatDeb},
		{name: "explicit rpm wins over deb distro", explicit: FormatRPM, raw: "ID=ubuntu\n", want: FormatRPM},
		{name: "unknown id falls back to dpkg", raw: "ID=weirdos\n", cmds: []string{"dpkg", "apt"}, want: FormatDeb},
		{name: "unknown id falls back to rpm", raw: "ID=weirdos\n", cmds: []string{"rpm", "yum"}, want: FormatRPM},
		{name: "both package managers is ambiguous", raw: "ID=weirdos\n", cmds: []string{"dpkg", "rpm"}, wantErr: true},
		{name: "unknown and nothing installed", raw: "ID=weirdos\n", wantErr: true},
		{name: "invalid explicit value", explicit: "tar.gz", raw: "ID=ubuntu\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			has := func(name string) bool {
				for _, c := range tc.cmds {
					if c == name {
						return true
					}
				}
				return false
			}
			got, err := DetectFormat(tc.explicit, ParseOSRelease([]byte(tc.raw)), has)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DetectFormat = %q, want an error", got)
				}
				if tc.explicit == "" && err.Error() == "" {
					t.Fatal("the error must explain how to choose a format")
				}
				return
			}
			if err != nil {
				t.Fatalf("DetectFormat: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DetectFormat = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectFormatErrorNamesTheFlag(t *testing.T) {
	_, err := DetectFormat(FormatAuto, ParseOSRelease([]byte("ID=weirdos\n")), func(string) bool { return false })
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"--pkg-format", "--cloudflared-path", "weirdos"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestArchFor(t *testing.T) {
	cases := map[string]struct{ deb, rpm string }{
		"amd64": {"amd64", "x86_64"},
		"arm64": {"arm64", "aarch64"},
	}
	for goarch, want := range cases {
		got, err := ArchFor(goarch)
		if err != nil {
			t.Fatalf("ArchFor(%s): %v", goarch, err)
		}
		if got.Deb != want.deb || got.RPM != want.rpm {
			t.Errorf("ArchFor(%s) = deb %q rpm %q, want %q/%q", goarch, got.Deb, got.RPM, want.deb, want.rpm)
		}
		if !got.DebSupported {
			t.Errorf("ArchFor(%s) should have a Debian package", goarch)
		}
	}
	// 386 exists as an RPM only; asking for a Debian package must fail loudly
	// rather than build a URL that 404s.
	got, err := ArchFor("386")
	if err != nil {
		t.Fatalf("ArchFor(386): %v", err)
	}
	if got.DebSupported {
		t.Error("386 has no Debian package")
	}
	if _, err := ArchFor("riscv64"); err == nil {
		t.Error("an unpublished architecture must be rejected")
	}
}
