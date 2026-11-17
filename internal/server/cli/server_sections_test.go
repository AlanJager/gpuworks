package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func loadSectionsFile(t *testing.T) serverconfig.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	content := "listen:\n  console: [127.0.0.1:8800]\nextensions:\n  quota:\n    key: /etc/k\n  billing: {}\n"
	if err := os.WriteFile(path, []byte(content), 0o664); err != nil {
		t.Fatal(err)
	}
	file, err := loadServerConfig(path, false, routeHost("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// serve hands the raw sections to the server, where the modules decode them.
func TestServerYAMLExtensionSectionsReachTheConfiguration(t *testing.T) {
	file := loadSectionsFile(t)
	cfg := app.DefaultConfig()
	applyServerConfig(&cfg, file, map[string]bool{})
	if len(cfg.Sections) != 2 {
		t.Fatalf("sections = %+v, want quota and billing", cfg.Sections)
	}
	var quota struct {
		Key string `yaml:"key"`
	}
	if err := file.Section("quota", &quota); err != nil || quota.Key != "/etc/k" {
		t.Fatalf("quota section = %+v, %v", quota, err)
	}
	node := cfg.Sections["quota"]
	if err := node.Decode(&quota); err != nil || quota.Key != "/etc/k" {
		t.Fatalf("the configuration's section = %+v, %v", quota, err)
	}
}

func TestUnclaimedSectionsDrawAWarningEach(t *testing.T) {
	file := loadSectionsFile(t)
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	warnUnclaimedSections(log, &file, Options{}.claimedSections())
	out := logged.String()
	if strings.Count(out, "level=WARN") != 2 || !strings.Contains(out, "section=billing") || !strings.Contains(out, "section=quota") {
		t.Fatalf("log = %q, want one warning per unclaimed section", out)
	}

	logged.Reset()
	warnUnclaimedSections(log, &file, []string{"quota"})
	if out := logged.String(); strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "section=billing") {
		t.Fatalf("log = %q, want only billing", out)
	}

	// Without server.yaml, or with no sections, nothing is logged.
	logged.Reset()
	warnUnclaimedSections(log, nil, nil)
	warnUnclaimedSections(log, &serverconfig.File{}, nil)
	if logged.Len() != 0 {
		t.Fatalf("log = %q, want nothing", logged.String())
	}
}
