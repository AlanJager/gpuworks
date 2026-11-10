package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseDocsArtifactRetention(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".gitlab-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var release struct {
		Artifacts struct {
			ExpireIn string `yaml:"expire_in"`
		}
		Rules  []struct{ If, When string }
		Script []string
	}
	// MR pipelines retain lightweight checks without installing a browser or
	// printing full handbooks. Only formal version tags produce PDF artifacts.
	var nodes map[string]yaml.Node
	if err := yaml.Unmarshal(data, &nodes); err != nil {
		t.Fatal(err)
	}
	if _, exists := nodes["docs-pdf-preview"]; exists {
		t.Fatal("MR pipelines must not generate PDF previews by default")
	}
	node := nodes["release-docs-pdf"]
	if err := node.Decode(&release); err != nil {
		t.Fatal(err)
	}
	if release.Artifacts.ExpireIn != "never" {
		t.Fatalf("release retention = %q, want never", release.Artifacts.ExpireIn)
	}
	if len(release.Rules) != 2 || release.Rules[0].If != `$CI_COMMIT_TAG =~ /^v[0-9]+\.[0-9]+\.[0-9]+$/` || release.Rules[1].When != "never" {
		t.Fatalf("permanent artifacts must only run for formal tags: %+v", release.Rules)
	}
	if !strings.Contains(strings.Join(release.Script, "\n"), `--version "$CI_COMMIT_TAG"`) {
		t.Fatal("release PDFs must use the formal version tag")
	}
}
