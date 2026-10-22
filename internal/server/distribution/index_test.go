package distribution

import (
	"encoding/json"
	"strings"
	"testing"
)

const goodSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func validIndex() Index {
	index := testIndex("nightly",
		ReleaseFile{OS: "linux", Arch: "amd64", File: File{Path: "releases/abc1234/gpuworks-abc1234-linux-amd64.tar.gz", Size: 10, SHA256: goodSHA}},
		ReleaseFile{OS: "linux", Arch: "arm64", File: File{Path: "releases/abc1234/gpuworks-abc1234-linux-arm64.tar.gz", Size: 11, SHA256: goodSHA}},
	)
	index.ImageCatalog = &CatalogFile{Version: "2026-10-08.ab12cd34", File: File{Path: "catalog/images/" + goodSHA + ".json", Size: 5, SHA256: goodSHA}}
	index.TemplateCatalog = &TemplateCatalog{
		CatalogFile:   CatalogFile{Version: "2026-09-14", File: File{Path: "catalog/templates/" + goodSHA + ".tar.gz", Size: 7, SHA256: goodSHA}},
		PackageDigest: goodSHA, SchemaVersion: 1,
	}
	return index
}

// The wire form is what CI writes and every released client reads; a renamed
// field would strand installed servers, so the keys are pinned literally.
func TestIndexWireJSON(t *testing.T) {
	raw := `{"schema":1,"channel":"nightly","published_at":"2026-10-11T02:00:00Z",
	  "server":{"release_id":"abc1234","commit":"abc1234def","built_at":"2026-10-11T01:00:00Z",
	    "files":[{"os":"linux","arch":"amd64","path":"releases/abc1234/x.tar.gz","size":10,"sha256":"` + goodSHA + `"}]},
	  "image_catalog":{"version":"2026-10-08.ab12cd34","path":"catalog/images/a.json","size":5,"sha256":"` + goodSHA + `"},
	  "template_catalog":{"version":"2026-09-14","package_digest":"` + goodSHA + `","schema_version":1,"path":"catalog/templates/a.tar.gz","size":7,"sha256":"` + goodSHA + `"},
	  "added_later":{"anything":true}}`
	index, err := ParseIndex([]byte(raw), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	asset, err := index.Server.Asset("linux", "amd64")
	if err != nil || asset.Path != "releases/abc1234/x.tar.gz" || asset.Size != 10 || asset.SHA256 != goodSHA {
		t.Fatalf("asset = %+v, %v", asset, err)
	}
	if index.ImageCatalog.Version != "2026-10-08.ab12cd34" || index.ImageCatalog.Path != "catalog/images/a.json" {
		t.Fatalf("image catalog = %+v", index.ImageCatalog)
	}
	if tc := index.TemplateCatalog; tc.PackageDigest != goodSHA || tc.SchemaVersion != 1 || tc.Size != 7 || tc.Version != "2026-09-14" {
		t.Fatalf("template catalog = %+v", tc)
	}

	out, err := json.Marshal(validIndex())
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(out, &generic); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "channel", "published_at", "server", "image_catalog", "template_catalog"} {
		if _, ok := generic[key]; !ok {
			t.Errorf("marshalled index lacks %q: %s", key, out)
		}
	}
	file := generic["server"].(map[string]any)["files"].([]any)[0].(map[string]any)
	for _, key := range []string{"os", "arch", "path", "size", "sha256"} {
		if _, ok := file[key]; !ok {
			t.Errorf("release file lacks %q: %v", key, file)
		}
	}
	template := generic["template_catalog"].(map[string]any)
	for _, key := range []string{"version", "package_digest", "schema_version", "path", "size", "sha256"} {
		if _, ok := template[key]; !ok {
			t.Errorf("template catalog lacks %q: %v", key, template)
		}
	}
}

func TestCatalogsAreOptional(t *testing.T) {
	index := validIndex()
	index.ImageCatalog, index.TemplateCatalog = nil, nil
	payload, _ := json.Marshal(index)
	if strings.Contains(string(payload), "image_catalog") || strings.Contains(string(payload), "template_catalog") {
		t.Fatalf("absent catalogs must be omitted: %s", payload)
	}
	if _, err := ParseIndex(payload, "nightly"); err != nil {
		t.Fatal(err)
	}
}

func TestParseIndexRefuses(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*Index)
		channel string
		want    string
	}{
		"other schema":          {mutate: func(x *Index) { x.Schema = 2 }, want: "schema 2"},
		"other channel":         {mutate: func(*Index) {}, channel: "release", want: `channel "nightly", not "release"`},
		"bad channel name":      {mutate: func(x *Index) { x.Channel = "Pre view" }, channel: "Pre view", want: "invalid"},
		"no release id":         {mutate: func(x *Index) { x.Server.ReleaseID = "" }, want: "release_id"},
		"release id with slash": {mutate: func(x *Index) { x.Server.ReleaseID = "a/b" }, want: "release_id"},
		"absolute path":         {mutate: func(x *Index) { x.Server.Files[0].Path = "/etc/passwd" }, want: "plain relative path"},
		"parent path":           {mutate: func(x *Index) { x.Server.Files[0].Path = "../x" }, want: "plain relative path"},
		"inner parent":          {mutate: func(x *Index) { x.Server.Files[0].Path = "releases/../x" }, want: "plain relative path"},
		"dot segment":           {mutate: func(x *Index) { x.Server.Files[0].Path = "./x" }, want: "plain relative path"},
		"backslash":             {mutate: func(x *Index) { x.Server.Files[0].Path = `releases\x` }, want: "plain relative path"},
		"url":                   {mutate: func(x *Index) { x.Server.Files[0].Path = "https://evil/x" }, want: "plain relative path"},
		"zero size":             {mutate: func(x *Index) { x.Server.Files[0].Size = 0 }, want: "no size"},
		"short sha":             {mutate: func(x *Index) { x.Server.Files[0].SHA256 = "abc" }, want: "sha256"},
		"upper-case sha":        {mutate: func(x *Index) { x.Server.Files[0].SHA256 = strings.ToUpper(goodSHA) }, want: "sha256"},
		"platform twice":        {mutate: func(x *Index) { x.Server.Files[1].Arch = "amd64" }, want: "twice"},
		"file without os":       {mutate: func(x *Index) { x.Server.Files[0].OS = "" }, want: "os/arch"},
		"image catalog version": {mutate: func(x *Index) { x.ImageCatalog.Version = " " }, want: "image catalog has no version"},
		"image catalog path":    {mutate: func(x *Index) { x.ImageCatalog.Path = "../m.json" }, want: "image catalog"},
		"template digest":       {mutate: func(x *Index) { x.TemplateCatalog.PackageDigest = "nope" }, want: "package_digest"},
		"template schema":       {mutate: func(x *Index) { x.TemplateCatalog.SchemaVersion = 0 }, want: "schema_version"},
		"template catalog sha":  {mutate: func(x *Index) { x.TemplateCatalog.SHA256 = "" }, want: "template catalog"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := validIndex()
			tc.mutate(&index)
			payload, err := json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			channel := tc.channel
			if channel == "" {
				channel = "nightly"
			}
			_, err = ParseIndex(payload, channel)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseIndex error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if _, err := ParseIndex([]byte("{"), "nightly"); err == nil {
		t.Fatal("a truncated payload must not parse")
	}
}

func TestAssetPicksExactPlatform(t *testing.T) {
	index := validIndex()
	got, err := index.Server.Asset("linux", "arm64")
	if err != nil || got.Size != 11 {
		t.Fatalf("Asset(linux, arm64) = %+v, %v", got, err)
	}
	if _, err := index.Server.Asset("darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "no darwin/arm64 build") {
		t.Fatalf("missing platform error = %v", err)
	}
}

func TestValidChannel(t *testing.T) {
	for _, ok := range []string{"nightly", "release", "beta-2"} {
		if !ValidChannel(ok) {
			t.Errorf("%q should be a valid channel", ok)
		}
	}
	for _, bad := range []string{"", "Stable", "2stable", "a/b", "..", strings.Repeat("a", 33)} {
		if ValidChannel(bad) {
			t.Errorf("%q should not be a valid channel", bad)
		}
	}
}

func TestNormalizeSources(t *testing.T) {
	got, err := NormalizeSources([]string{" https://a.example/dist/ ", "https://a.example/dist", "http://b.example"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "https://a.example/dist,http://b.example" {
		t.Fatalf("NormalizeSources = %v", got)
	}
	for _, bad := range []string{"ftp://a", "https://", "a.example", "https://a.example/?x=1", "https://a.example/#f"} {
		if _, err := NormalizeSource(bad); err == nil {
			t.Errorf("NormalizeSource(%q) should fail", bad)
		}
	}
}

func TestDefaultSourcesAndKeys(t *testing.T) {
	sources, err := (&Client{}).sources()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sources, ",") != strings.Join(DefaultSources, ",") || len(sources) != 3 {
		t.Fatalf("default sources = %v", sources)
	}
	for _, source := range DefaultSources {
		if !strings.HasPrefix(source, "https://") {
			t.Errorf("default source %s must be https", source)
		}
	}
	if key := TrustedKeys()["gpuworks-dist-1"]; len(key) != 32 {
		t.Fatalf("built-in key gpuworks-dist-1 = %v", key)
	}
}

func TestKnownChannels(t *testing.T) {
	if !KnownChannel("nightly") || !KnownChannel("release") || KnownChannel("preview") || KnownChannel("") {
		t.Fatal("the published channels are nightly and release")
	}
}

func TestServerReleaseNewerThan(t *testing.T) {
	release := ServerRelease{ReleaseID: "v1.3.0", BuiltAt: "2026-10-11T10:00:00+08:00"}
	for _, tc := range []struct {
		id, built string
		want      bool
	}{
		{"v1.3.0", "2026-01-01T00:00:00Z", false},  // the same release
		{"abc1234", "2026-10-11T01:00:00Z", true},  // an older nightly (09:00 CST)
		{"def5678", "2026-10-11T03:00:00Z", false}, // a nightly built after the release
		{"dev", "unknown", true},                   // a local build
		{"abc1234", "2026-10-11T02:00:00Z", false}, // the same instant is not newer
	} {
		if got := release.NewerThan(tc.id, tc.built); got != tc.want {
			t.Errorf("NewerThan(%s, %s) = %v, want %v", tc.id, tc.built, got, tc.want)
		}
	}
	if !(ServerRelease{ReleaseID: "x", BuiltAt: "garbage"}).NewerThan("y", "2026-10-11T00:00:00Z") {
		t.Fatal("an unreadable release time must not hide an update")
	}
}

func TestServerReleaseOlderThan(t *testing.T) {
	release := ServerRelease{ReleaseID: "v1.3.0", BuiltAt: "2026-10-11T10:00:00+08:00"}
	for _, tc := range []struct {
		built string
		want  bool
	}{
		{"2026-10-11T03:00:00Z", true},  // a server built after the release
		{"2026-10-11T02:00:00Z", false}, // the same instant
		{"2026-10-11T01:00:00Z", false}, // a server built before it
		{"unknown", false},              // a local build: nothing is known
	} {
		if got := release.OlderThan(tc.built); got != tc.want {
			t.Errorf("OlderThan(%s) = %v, want %v", tc.built, got, tc.want)
		}
	}
	if (ServerRelease{BuiltAt: "garbage"}).OlderThan("2026-10-11T00:00:00Z") {
		t.Fatal("an unreadable release time must not hide catalog updates")
	}
}
