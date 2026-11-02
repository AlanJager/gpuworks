package goldenimage

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestPrepareModelCommandNeverPullsAndHonoursThePrivilegePrefix(t *testing.T) {
	files := []File{{Path: "config.json", URL: "https://example.invalid/config.json", Size: 1}}
	for _, sudo := range []string{"", "sudo -n "} {
		command := PrepareModelCommand(DefaultModelPath, DefaultDockerImage, files, sudo)
		for _, want := range []string{sudo + "docker image inspect", sudo + "python3 /tmp/gpuworks-prepare-model.py", sudo + "sync"} {
			if !strings.Contains(command, want) {
				t.Errorf("sudo=%q: command lacks %q", sudo, want)
			}
		}
		if strings.Contains(command, "docker pull") {
			t.Errorf("sudo=%q: the model step must never pull an image", sudo)
		}
	}
	if command := PrepareModelCommand(DefaultModelPath, DefaultDockerImage, files, ""); strings.Contains(command, "sudo") {
		t.Errorf("a root builder must not need sudo: %s", command)
	}
}

func TestPrepareModelCommandEmbedsTheManifestAndAChecksumWritingScript(t *testing.T) {
	command := PrepareModelCommand("/m", "img", []File{{Path: "a/b.safetensors", URL: "https://x/y", Size: 7}}, "")
	fields := strings.Split(command, "'")
	var decoded []string
	for _, field := range fields {
		if raw, err := base64.StdEncoding.DecodeString(field); err == nil && len(field) > 16 {
			decoded = append(decoded, string(raw))
		}
	}
	joined := strings.Join(decoded, "\n")
	for _, want := range []string{"SHA256SUMS", "unsafe model path", "a/b.safetensors", `"size":7`} {
		if !strings.Contains(joined, want) {
			t.Errorf("embedded script or manifest lacks %q", want)
		}
	}
}

func TestValidationCommandChecksWeightsChecksumsAndTheImage(t *testing.T) {
	command := ValidationCommand("/opt/m", "gpustack/runner:x", "sudo -n ")
	for _, want := range []string{"sudo -n sh -c", "config.json", "*.safetensors", "sha256sum -c SHA256SUMS", "docker image inspect"} {
		if !strings.Contains(command, want) {
			t.Errorf("validation lacks %q: %s", want, command)
		}
	}
}

func TestShellQuoteSurvivesQuotes(t *testing.T) {
	if got := ShellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("ShellQuote = %s", got)
	}
}
