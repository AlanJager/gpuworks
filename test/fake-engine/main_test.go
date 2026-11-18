package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBinaryResponseEnvironment exercises the same entry point and environment
// as the container image, rather than supplying Options directly to a handler.
func TestBinaryResponseEnvironment(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "fake-engine")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	for _, reasoning := range []bool{false, true} {
		t.Run(fmt.Sprintf("reasoning=%t", reasoning), func(t *testing.T) {
			dir := t.TempDir()
			model := filepath.Join(dir, "model")
			if err := os.Mkdir(model, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(model, "config.json"), []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			log, err := os.Create(filepath.Join(dir, "engine.log"))
			if err != nil {
				t.Fatal(err)
			}
			servedModel := fmt.Sprintf("env-model-%d-%d", os.Getpid(), port)
			cmd := exec.Command(binary, "--model", model, "--served-model-name", servedModel, "--port", fmt.Sprint(port))
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "FAKE_ENGINE_") && !strings.HasPrefix(value, "TMPDIR=") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			const content = "configured chat\n中文 \"content\""
			const thought = "first think\nthen answer"
			cmd.Env = append(cmd.Env, "TMPDIR="+dir, "FAKE_ENGINE_RESPONSE_TEXT="+content, "FAKE_ENGINE_PROMPT_TOKENS=19", "FAKE_ENGINE_COMPLETION_TOKENS=23")
			if reasoning {
				cmd.Env = append(cmd.Env, "FAKE_ENGINE_REASONING_TEXT="+thought)
			}
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				log.Close()
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited; log.Close() })
			client := &http.Client{Timeout: 5 * time.Second}
			base := fmt.Sprintf("http://127.0.0.1:%d", port)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			ready := false
			for !ready {
				select {
				case <-ctx.Done():
					data, _ := os.ReadFile(log.Name())
					t.Fatalf("binary readiness: %v\n%s", ctx.Err(), data)
				case err := <-exited:
					exited <- err
					data, _ := os.ReadFile(log.Name())
					t.Fatalf("binary exited: %v\n%s", err, data)
				case <-ticker.C:
					response, err := client.Get(base + "/v1/models")
					if err == nil {
						var body map[string]any
						err = json.NewDecoder(response.Body).Decode(&body)
						response.Body.Close()
						ready = err == nil && strings.Contains(fmt.Sprint(body), servedModel)
					}
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "fake-engine-argv.json")); err != nil {
				t.Fatalf("binary did not use owned TMPDIR: %v", err)
			}
			t.Logf("owned fake-engine pid=%d url=%s", cmd.Process.Pid, base)
			t.Cleanup(client.CloseIdleConnections)
			for _, endpoint := range []string{"/v1/chat/completions", "/v1/completions"} {
				for _, stream := range []bool{false, true} {
					body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"prompt":"hi","stream":%t`, servedModel, stream)
					if stream {
						body += `,"stream_options":{"include_usage":true}`
					}
					body += `}`
					response, err := client.Post(base+endpoint, "application/json", strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					var finalContent, reasoningContent strings.Builder
					if stream {
						if response.Header.Get("Content-Type") != "text/event-stream" {
							t.Fatalf("stream headers=%v", response.Header)
						}
						scanner := bufio.NewScanner(response.Body)
						done := false
						contentStarted := false
						for scanner.Scan() {
							line := scanner.Text()
							if !strings.HasPrefix(line, "data: ") {
								continue
							}
							data := strings.TrimPrefix(line, "data: ")
							if data == "[DONE]" {
								done = true
								continue
							}
							var event struct {
								Choices []struct {
									Delta struct {
										Content   string `json:"content"`
										Reasoning string `json:"reasoning_content"`
									} `json:"delta"`
									Text string `json:"text"`
								} `json:"choices"`
							}
							if err := json.Unmarshal([]byte(data), &event); err != nil {
								t.Fatal(err)
							}
							for _, choice := range event.Choices {
								if endpoint == "/v1/completions" {
									finalContent.WriteString(choice.Text)
									continue
								}
								if choice.Delta.Reasoning != "" {
									if contentStarted {
										t.Fatal("reasoning followed final content")
									}
									reasoningContent.WriteString(choice.Delta.Reasoning)
								}
								if choice.Delta.Content != "" {
									contentStarted = true
									finalContent.WriteString(choice.Delta.Content)
								}
							}
						}
						if err := scanner.Err(); err != nil {
							t.Fatal(err)
						}
						if !done {
							t.Fatal("stream missing DONE")
						}
					} else {
						var event struct {
							Choices []struct {
								Message struct {
									Content   string `json:"content"`
									Reasoning string `json:"reasoning_content"`
								} `json:"message"`
								Text string `json:"text"`
							} `json:"choices"`
							Usage map[string]int `json:"usage"`
						}
						if err := json.NewDecoder(response.Body).Decode(&event); err != nil {
							t.Fatal(err)
						}
						if len(event.Choices) != 1 || event.Usage["prompt_tokens"] != 19 || event.Usage["completion_tokens"] != 23 {
							t.Fatalf("sync body=%+v", event)
						}
						if endpoint == "/v1/completions" {
							finalContent.WriteString(event.Choices[0].Text)
						} else {
							finalContent.WriteString(event.Choices[0].Message.Content)
						}
						reasoningContent.WriteString(event.Choices[0].Message.Reasoning)
					}
					response.Body.Close()
					wantThought := ""
					if reasoning && endpoint == "/v1/chat/completions" {
						wantThought = thought
					}
					if finalContent.String() != content || reasoningContent.String() != wantThought {
						t.Fatalf("stream=%v content=%q reasoning=%q, want content=%q reasoning=%q", stream, finalContent.String(), reasoningContent.String(), content, wantThought)
					}
				}
			}
		})
	}
}
