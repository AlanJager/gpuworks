package templateprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPTaskEvaluatorTextAndRepeatedOutput(t *testing.T) {
	response := "The answer is 4."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer data-key" {
			t.Errorf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["temperature"] != float64(0) || body["model"] != "served-model" {
			t.Errorf("body = %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": response}}}})
	}))
	defer server.Close()

	evaluator := HTTPTaskEvaluator{Client: server.Client(), APIKey: "data-key"}
	observation := readyObservation()
	observation.BaseURL = server.URL
	result, err := evaluator.Evaluate(context.Background(), observation, validTextCase())
	if err != nil || !result.RequestCompleted || !result.SemanticPassed || result.HTTPStatus != 200 {
		t.Fatalf("result=%+v err=%v", result, err)
	}

	response = "!!!!!!!!!!!!"
	result, err = evaluator.Evaluate(context.Background(), observation, validTextCase())
	if err != nil || !result.RequestCompleted || result.SemanticPassed || !strings.Contains(result.Warning, "repeated") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestHTTPTaskEvaluatorEmbeddingRanking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"embedding": []float64{1, 0}},
			map[string]any{"embedding": []float64{0.99, 0.01}},
			map[string]any{"embedding": []float64{0, 1}},
		}})
	}))
	defer server.Close()
	observation := readyObservation()
	observation.BaseURL = server.URL
	result, err := (HTTPTaskEvaluator{Client: server.Client()}).Evaluate(context.Background(), observation, L3Case{
		CaseID: "embedding", Task: L3TaskEmbedding,
		EmbeddingInputs: []string{"same", "same", "different"}, ExpectedDimensions: 2, MinCosineMargin: 0.02,
	})
	if err != nil || !result.SemanticPassed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := result.Verification["embedding_dimensions"]; got != 2 {
		t.Fatalf("dimension = %#v", got)
	}
	if margin, _ := result.Verification["cosine_margin"].(float64); margin < 0.02 || math.IsNaN(margin) {
		t.Fatalf("margin = %#v", result.Verification["cosine_margin"])
	}
}

func TestHTTPTaskEvaluatorASRAndTTSBackread(t *testing.T) {
	wav := testWAV([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/speech":
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(wav)
		case "/v1/audio/transcriptions":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart: %v", err)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("file: %v", err)
			} else {
				body, _ := io.ReadAll(file)
				if !bytes.Equal(body, wav) {
					t.Errorf("audio body differs")
				}
				_ = file.Close()
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"text": "The quick brown fox"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	wavPath := filepath.Join(dir, "fixture.wav")
	if err := os.WriteFile(wavPath, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	observation := readyObservation()
	observation.BaseURL = server.URL
	evaluator := HTTPTaskEvaluator{Client: server.Client()}
	for _, testCase := range []L3Case{
		{CaseID: "asr", Task: L3TaskASR, AudioPath: wavPath, ExpectedTranscript: "the quick brown fox", MaxWER: 0},
		{CaseID: "tts", Task: L3TaskTTS, SpeechText: "The quick brown fox", RequestOverrides: map[string]any{"voice": "alloy", "response_format": "wav"}, ASRBaseURL: server.URL, ASRModel: "asr", ExpectedTranscript: "the quick brown fox", MaxWER: 0},
	} {
		result, err := evaluator.Evaluate(context.Background(), observation, testCase)
		if err != nil || !result.RequestCompleted || !result.SemanticPassed {
			t.Fatalf("%s result=%+v err=%v", testCase.Task, result, err)
		}
		if got := result.Verification["word_error_rate"]; got != float64(0) {
			t.Fatalf("%s WER = %#v", testCase.Task, got)
		}
	}
}

func TestHTTPTaskEvaluatorVisionLanguage(t *testing.T) {
	var sawDataURL bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sawDataURL = strings.Contains(string(body), "data:image/png;base64,")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"HYGON OCR 4827"}}]}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "fixture.png")
	if err := os.WriteFile(imagePath, []byte("\x89PNG\r\n\x1a\nfixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	observation := readyObservation()
	observation.BaseURL = server.URL
	result, err := (HTTPTaskEvaluator{Client: server.Client()}).Evaluate(context.Background(), observation, L3Case{
		CaseID: "vl", Task: L3TaskVisionLanguage, ImagePath: imagePath,
		Prompt: "OCR the image", ExpectedKeywords: []string{"HYGONOCR4827"},
	})
	if err != nil || !result.SemanticPassed || !sawDataURL {
		t.Fatalf("result=%+v sawDataURL=%v err=%v", result, sawDataURL, err)
	}
}

func TestHTTPTaskEvaluatorHTTPFailureIsPlatformFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "engine failed", http.StatusInternalServerError)
	}))
	defer server.Close()
	observation := readyObservation()
	observation.BaseURL = server.URL
	result, err := (HTTPTaskEvaluator{Client: server.Client()}).Evaluate(context.Background(), observation, validTextCase())
	if err != nil || result.RequestCompleted || result.SemanticPassed || result.HTTPStatus != 500 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func testWAV(data []byte) []byte {
	buf := &bytes.Buffer{}
	buf.WriteString("RIFF")
	writeLE32(buf, uint32(36+len(data)))
	buf.WriteString("WAVEfmt ")
	writeLE32(buf, 16)
	buf.Write([]byte{1, 0, 1, 0})
	writeLE32(buf, 16000)
	writeLE32(buf, 32000)
	buf.Write([]byte{2, 0, 16, 0})
	buf.WriteString("data")
	writeLE32(buf, uint32(len(data)))
	buf.Write(data)
	return buf.Bytes()
}

func writeLE32(w io.Writer, value uint32) {
	_, _ = w.Write([]byte{byte(value), byte(value >> 8), byte(value >> 16), byte(value >> 24)})
}
