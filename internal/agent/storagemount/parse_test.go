package storagemount

import (
	"strings"
	"testing"
)

const sampleMountinfo = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro
36 35 98:0 /mnt1 /mnt/with\040space rw,noatime master:1 - nfs4 nfs.example:/export\040dir rw,vers=4.2,rsize=1048576
40 22 0:50 / /mnt/plain rw,relatime - nfs 10.0.0.5:/data/ rw,vers=3,proto=tcp
41 22 0:51 / /mnt/two ro shared:5 master:6 propagate_from:7 - nfs4 a:/x ro,vers=4.1
garbage line
42 22 0:52 / /mnt/nosep rw shared:1 nfs4 a:/x rw
43 22 0:53 / /mnt/nosuper rw - tmpfs tmpfs
44 22 0:54 / /mnt/two ro - nfs4 b:/y ro,vers=4.0
`

func TestParseMountinfo(t *testing.T) {
	got, err := ParseMountinfo(strings.NewReader(sampleMountinfo))
	if err != nil {
		t.Fatal(err)
	}
	want := []MountEntry{
		{"/", "ext4", "/dev/sda1", "rw,relatime", "rw,errors=remount-ro"},
		{"/mnt/with space", "nfs4", "nfs.example:/export dir", "rw,noatime", "rw,vers=4.2,rsize=1048576"},
		{"/mnt/plain", "nfs", "10.0.0.5:/data/", "rw,relatime", "rw,vers=3,proto=tcp"},
		{"/mnt/two", "nfs4", "a:/x", "ro", "ro,vers=4.1"},
		{"/mnt/nosuper", "tmpfs", "tmpfs", "rw", ""},
		{"/mnt/two", "nfs4", "b:/y", "ro", "ro,vers=4.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUnescapeOctal(t *testing.T) {
	for in, want := range map[string]string{
		`a\040b`:  "a b",
		`a\134b`:  `a\b`,
		`a\011b`:  "a\tb",
		`a\04`:    `a\04`, // truncated escape stays literal
		`a\9zz`:   `a\9zz`,
		`plain`:   "plain",
		`end\040`: "end ",
	} {
		if got := unescapeOctal(in); got != want {
			t.Errorf("unescapeOctal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindMount(t *testing.T) {
	entries, _ := ParseMountinfo(strings.NewReader(sampleMountinfo))
	e, ok := FindMount(entries, "/mnt/two")
	if !ok || e.Source != "b:/y" {
		t.Errorf("stacked mounts: last entry must win, got %+v ok=%v", e, ok)
	}
	if e, ok := FindMount(entries, "/mnt/with space/"); !ok || e.FSType != "nfs4" {
		t.Errorf("trailing slash and decoded space must match, got %+v ok=%v", e, ok)
	}
	if _, ok := FindMount(entries, "/mnt"); ok {
		t.Error("a parent directory of a mount point is not a match")
	}
	if _, ok := FindMount(entries, "/mnt/plain/sub"); ok {
		t.Error("a path below a mount point is not a match")
	}
	if _, ok := FindMount(nil, "/x"); ok {
		t.Error("empty mount table must not match")
	}
}

func TestSuperOptionAndMerge(t *testing.T) {
	if v := superOption("rw,vers=4.2,rsize=1", "vers"); v != "4.2" {
		t.Errorf("vers = %q", v)
	}
	if v := superOption("rw,nfsvers=4", "vers"); v != "" {
		t.Errorf("must not match a longer name, got %q", v)
	}
	if got := mergeOptions("rw,relatime", "rw,vers=4.2", ""); got != "rw,relatime,vers=4.2" {
		t.Errorf("merge = %q", got)
	}
}
