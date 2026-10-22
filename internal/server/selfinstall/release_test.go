package selfinstall

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "", wantErr: "needs a URL"},
		{in: "https://gpuworks.zstack.ai/", want: "https://gpuworks.zstack.ai"},
		{in: "  http://mirror.internal:8080/releases  ", want: "http://mirror.internal:8080/releases"},
		{in: "ftp://gpuworks.zstack.ai", wantErr: "scheme must be http or https"},
		{in: "/var/lib/releases", wantErr: "scheme must be http or https"},
		{in: "https://", wantErr: "no host"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := NormalizeBaseURL(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeBaseURL: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReleaseAssetSelection(t *testing.T) {
	release := Release{ReleaseID: "v1.0.0", Files: []ReleaseFile{
		{OS: "linux", Arch: "linux-amd64", GoArch: "amd64", Name: "linux-amd64.tar.gz"},
		{OS: "linux", Arch: "linux-arm64", GoArch: "arm64", Name: "linux-arm64.tar.gz"},
		{OS: "darwin", Arch: "macos-arm64", GoArch: "arm64", Name: "darwin-arm64.tar.gz"},
		// A manifest from before goarch existed: the site's own arch key is the
		// only thing to match.
		{OS: "darwin", Arch: "macos-amd64", Name: "legacy-darwin-amd64.tar.gz"},
	}}
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "linux-amd64.tar.gz"},
		{"linux", "arm64", "linux-arm64.tar.gz"},
		{"darwin", "arm64", "darwin-arm64.tar.gz"},
		{"darwin", "amd64", "legacy-darwin-amd64.tar.gz"},
	} {
		file, err := release.Asset(tc.goos, tc.goarch)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.goos, tc.goarch, err)
		}
		if file.Name != tc.want {
			t.Fatalf("%s/%s → %q, want %q", tc.goos, tc.goarch, file.Name, tc.want)
		}
	}
	if _, err := release.Asset("windows", "amd64"); err == nil {
		t.Fatal("an unsupported platform was accepted")
	}
}

func TestAssetURLRefusesToEscapeTheDownloadDirectory(t *testing.T) {
	for _, name := range []string{"", "../../etc/passwd", "sub/dir.tar.gz", `windows\path.tar.gz`, "."} {
		if _, err := assetURL("https://gpuworks.zstack.ai", ReleaseFile{Name: name}); err == nil {
			t.Fatalf("artifact name %q was accepted", name)
		}
	}
	url, err := assetURL("https://gpuworks.zstack.ai", ReleaseFile{Name: "gpuworks-v1.0.0-linux-amd64.tar.gz"})
	if err != nil {
		t.Fatalf("assetURL: %v", err)
	}
	if url != "https://gpuworks.zstack.ai/downloads/gpuworks-v1.0.0-linux-amd64.tar.gz" {
		t.Fatalf("url = %q", url)
	}
}

func TestFetchReleaseReportsWhatWentWrong(t *testing.T) {
	h := newHarness(t, t.TempDir())
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"missing":     func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
		"not-json":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<!doctype html>")) },
		"no-release-": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"commit":"abc"}`)) },
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(handler))
			defer srv.Close()
			h.deps.Client = srv.Client()
			h.installer = New(h.layout, h.deps, h.io)

			_, err := h.installer.fetchRelease(context.Background(), srv.URL)
			if err == nil {
				t.Fatal("a broken release manifest was accepted")
			}
			if !strings.Contains(err.Error(), releasePath) && !strings.Contains(err.Error(), "release_id") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestFetchReleaseReadsTheSiteManifest(t *testing.T) {
	h := newHarness(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		payload, _ := json.Marshal(Release{ReleaseID: "v9.9.9", Channel: "preview",
			Files: []ReleaseFile{{OS: "linux", GoArch: "amd64", Name: "x.tar.gz"}}})
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	h.deps.Client = srv.Client()
	h.installer = New(h.layout, h.deps, h.io)

	release, err := h.installer.fetchRelease(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchRelease: %v", err)
	}
	if release.ReleaseID != "v9.9.9" || release.Channel != "preview" || len(release.Files) != 1 {
		t.Fatalf("release = %+v", release)
	}
}

func TestReadInstallationComesFromTheMachine(t *testing.T) {
	h := newHarness(t, t.TempDir())
	existing := h.writeExistingInstall("v1.0.0")
	inst, err := h.installer.readInstallation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inst.Mode != ModeSystemd || inst.ServiceUser != DefaultServiceUser || inst.ReleaseID != "v1.0.0" ||
		inst.Config.Listen.Console[0] != existing.Addr || inst.Config.DataDir != h.layout.DataDir {
		t.Fatalf("installation = %+v", inst)
	}
	// A unit that starts another installation's binary is not this one's.
	writeFile(t, h.layout.UnitPath(), RenderUnit(UnitConfig{Binary: "/opt/other/gpuworks", ConfigPath: "/opt/other/server.yaml", DataDir: "/opt/other/data", ServiceUser: "root"}), 0o644)
	if inst, _ := h.installer.readInstallation(context.Background()); inst.Mode != ModeNohup || inst.ServiceUser != "" {
		t.Fatalf("another installation's unit was claimed: %+v", inst)
	}
	// A binary that cannot report its version leaves the release unknown.
	h.run.mu.Lock()
	delete(h.run.output, h.layout.Binary()+" version")
	h.run.mu.Unlock()
	h.run.fail[h.layout.Binary()+" version"] = errors.New("exec format error")
	if inst, _ := h.installer.readInstallation(context.Background()); inst.ReleaseID != "" {
		t.Fatalf("release = %q", inst.ReleaseID)
	}
}

func TestReadInstallationExplainsHowToAdoptADirectory(t *testing.T) {
	h := newHarness(t, t.TempDir())
	_, err := h.installer.readInstallation(context.Background())
	if !errors.Is(err, ErrNotInstalled) || !strings.Contains(err.Error(), "gpuworks install --dir "+h.layout.Dir) {
		t.Fatalf("err = %v", err)
	}
	writeFile(t, h.layout.ConfigPath(), "listen:\n  consle: []\n", 0o664)
	if _, err := h.installer.readInstallation(context.Background()); err == nil || errors.Is(err, ErrNotInstalled) {
		t.Fatalf("a damaged server.yaml was not reported: %v", err)
	}
}

func TestHistoryAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install-history.jsonl")
	entries := []HistoryEntry{
		{Action: ActionInstall, Result: "ok", ReleaseID: "v1.0.0", Mode: ModeSystemd, By: "alice"},
		{Action: ActionUpgrade, Result: "failed", ReleaseID: "v2.0.0", PreviousReleaseID: "v1.0.0",
			Backup: "/opt/gpuworks/server/backups/v1.0.0.tar.gz", Error: "unhealthy"},
	}
	for _, entry := range entries {
		if err := AppendHistory(path, entry, 0o600); err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}

	// A crash mid-write leaves a partial line; it must not hide the rest.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"action":"upgrade","resu`); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	got, err := ReadHistory(path)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("history = %+v", got)
	}
	for i, entry := range got {
		if entry.Action != entries[i].Action || entry.ReleaseID != entries[i].ReleaseID {
			t.Fatalf("entry %d = %+v", i, entry)
		}
		if entry.At == "" {
			t.Fatalf("entry %d has no timestamp", i)
		}
	}
	if got[1].Error != "unhealthy" || got[1].Backup != entries[1].Backup {
		t.Fatalf("entry 1 = %+v", got[1])
	}
}

func TestPromptNeedsATerminal(t *testing.T) {
	io := &IO{In: strings.NewReader("yes\n"), Out: &strings.Builder{}, Err: &strings.Builder{},
		IsTerminal: func() bool { return false }}
	if _, err := io.Prompt("continue? "); err == nil {
		t.Fatal("a prompt was answered without a terminal")
	}
	if _, err := io.PromptPassword("password: "); err == nil {
		t.Fatal("a password prompt was answered without a terminal")
	}
	if _, err := io.Confirm("continue?", true); err == nil {
		t.Fatal("a confirmation was answered without a terminal")
	}
}

func TestConfirmAnswers(t *testing.T) {
	var out strings.Builder
	cases := []struct {
		answer string
		want   bool
		wantIn bool
		err    bool
	}{
		{answer: "", want: true, wantIn: true},
		{answer: "y\n", want: true, wantIn: true},
		{answer: "Y\n", want: true, wantIn: true},
		{answer: "n\n", want: false, wantIn: true},
		{answer: "no\n", want: false, wantIn: true},
		{answer: "maybe\n", wantIn: true, err: true},
	}
	for _, tc := range cases {
		io := &IO{In: strings.NewReader(tc.answer), Out: &out, Err: &out,
			IsTerminal: func() bool { return true }}
		got, err := io.Confirm("install?", tc.wantIn)
		if tc.err {
			if err == nil {
				t.Fatalf("%q: accepted", tc.answer)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tc.answer, err)
		}
		if got != tc.want {
			t.Fatalf("%q → %v, want %v", tc.answer, got, tc.want)
		}
	}
	if !strings.Contains(out.String(), "install?") {
		t.Fatalf("the question was not asked: %q", out.String())
	}
}

func TestServiceUserDetection(t *testing.T) {
	if userExists("") {
		t.Fatal("an empty user name must not resolve")
	}
	if userExists("definitely-not-a-real-account-4f2a") {
		t.Fatal("a missing account resolved")
	}
	if !userExists(currentUserName()) {
		t.Fatalf("the invoking user %q was not found", currentUserName())
	}
	if got := currentUserName(); got == "" {
		t.Fatal("currentUserName is empty")
	}
}

func TestNologinShellExists(t *testing.T) {
	shell := nologinShell()
	if !fileExists(shell) {
		t.Fatalf("the service account shell %q does not exist", shell)
	}
}
