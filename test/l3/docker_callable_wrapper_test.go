//go:build docker

package l3

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const callableWrapperImage = "gpuworks/callable-wrapper-fixture:test"

func TestDockerCallableWrappersComeOnlyFromReadOnlyCatalogAssets(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := ensureCallableWrapperImage(root); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	// This filesystem preflight needs no network, like the wrapper cases below.
	if output, err := exec.Command("docker", "run", "--rm", "--network", "none", "--entrypoint", "sh", callableWrapperImage, "-c", "test ! -e /opt/gpuworks/runtime/gpuworks_serve.py").CombinedOutput(); err != nil {
		t.Fatalf("fixture image unexpectedly contains the wrapper: %v: %s", err, output)
	}
	cases := []struct {
		name, adapter, operation, config string
		request                          func(*testing.T, string) []byte
		want                             []byte
	}{
		{name: "transformers", adapter: "transformers_adapter:create", operation: "openai.embeddings", config: `{"task":"feature-extraction"}`, request: postWrapperJSON("/v1/embeddings", `{"input":"hello"}`), want: []byte(`"embedding":[0.125,0.25,0.5]`)},
		{name: "funasr", adapter: "funasr_adapter:create", operation: "openai.audio.transcribe", config: `{}`, request: postWrapperAudio, want: []byte(`"text":"container-transcript:RIFF"`)},
		{name: "diffusers", adapter: "diffusers_adapter:create", operation: "openai.image.generate", config: `{}`, request: postWrapperJSON("/v1/images/generations", `{"prompt":"sunrise"}`), want: []byte(`"b64_json":"UE5HLWNvbnRhaW5lcg=="`)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			container := startCallableWrapperContainer(t, root, test.adapter, test.operation, test.config)
			body := test.request(t, container)
			if !bytes.Contains(body, test.want) {
				t.Fatalf("response=%s want=%s", body, test.want)
			}
			inspect := exec.Command("docker", "inspect", "--format", `{{range .Mounts}}{{if eq .Destination "/opt/gpuworks/runtime"}}{{.RW}}{{end}}{{end}}`, container)
			if output, err := inspect.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "false" {
				t.Fatalf("runtime asset mount is not read-only: output=%q err=%v", output, err)
			}
			if output, err := exec.Command("docker", "stop", "--time", "2", container).CombinedOutput(); err != nil {
				t.Fatalf("SIGTERM stop: %v: %s", err, output)
			}
			if output, err := exec.Command("docker", "inspect", "--format", `{{.State.Running}}`, container).CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "false" {
				t.Fatalf("wrapper did not exit after SIGTERM: output=%q err=%v", output, err)
			}
		})
	}
}

func ensureCallableWrapperImage(root string) error {
	if exec.Command("docker", "image", "inspect", callableWrapperImage).Run() == nil {
		return nil
	}
	args := []string{"build", "--add-host", "host.docker.internal:host-gateway", "-t", callableWrapperImage}
	if base := os.Getenv("GPUWORKS_L3_RUNTIME_BASE_IMAGE"); base != "" {
		args = append(args, "--build-arg", "GPUWORKS_L3_RUNTIME_BASE_IMAGE="+base)
	}
	for _, key := range []string{"http_proxy", "https_proxy"} {
		value := os.Getenv(key)
		if value == "" {
			value = os.Getenv(strings.ToUpper(key))
		}
		value = strings.ReplaceAll(value, "127.0.0.1", "host.docker.internal")
		value = strings.ReplaceAll(value, "localhost", "host.docker.internal")
		if value != "" {
			args = append(args, "--build-arg", key+"="+value)
		}
	}
	args = append(args, filepath.Join(root, "test/runtime-wrapper-image"))
	command := exec.Command("docker", args...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("build callable wrapper fixture: %w: %s", err, output)
	}
	return nil
}

func startCallableWrapperContainer(t *testing.T, root, adapter, operation, config string) string {
	t.Helper()
	assets := filepath.Join(root, "test/runtime-wrapper-assets")
	command := exec.Command("docker", "run", "-d", "--network", "none", "-v", assets+":/opt/gpuworks/runtime:ro", callableWrapperImage,
		"/opt/gpuworks/runtime/gpuworks_serve.py", "--adapter", adapter, "--model", "fixture-model", "--operation", operation, "--adapter-config", config, "--port", "8000")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("start callable wrapper: %v: %s", err, output)
	}
	container := strings.TrimSpace(string(output))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, requestErr := exec.Command("docker", "exec", container, "python3", "-c", wrapperGETScript("/health")).CombinedOutput()
		if requestErr == nil && bytes.Contains(body, []byte(`"status":"ready"`)) {
			return container
		}
		time.Sleep(50 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
	t.Fatalf("wrapper did not become healthy: %s", logs)
	return ""
}

func postWrapperJSON(path, payload string) func(*testing.T, string) []byte {
	return func(t *testing.T, container string) []byte {
		t.Helper()
		script := fmt.Sprintf("import urllib.request; r=urllib.request.Request('http://127.0.0.1:8000%s',data=%q.encode(),headers={'Content-Type':'application/json'},method='POST'); print(urllib.request.urlopen(r).read().decode())", path, payload)
		body, err := exec.Command("docker", "exec", container, "python3", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("container request: %v: %s", err, body)
		}
		return body
	}
}

func postWrapperAudio(t *testing.T, container string) []byte {
	t.Helper()
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	_ = writer.WriteField("model", "fixture-model")
	part, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("RIFF-audio"))
	_ = writer.Close()
	encodedPayload, _ := json.Marshal(base64.StdEncoding.EncodeToString(payload.Bytes()))
	encodedType, _ := json.Marshal(writer.FormDataContentType())
	script := fmt.Sprintf("import base64,urllib.request; data=base64.b64decode(%s); r=urllib.request.Request('http://127.0.0.1:8000/v1/audio/transcriptions',data=data,headers={'Content-Type':%s},method='POST'); print(urllib.request.urlopen(r).read().decode())", encodedPayload, encodedType)
	body, err := exec.Command("docker", "exec", container, "python3", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("container multipart request: %v: %s", err, body)
	}
	return body
}

func wrapperGETScript(path string) string {
	return fmt.Sprintf("import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8000%s').read().decode())", path)
}
