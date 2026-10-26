package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestDiscoveryDescribesTheTranscriptionHandler(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model"}))
	defer server.Close()
	response, err := http.Get(server.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var document struct {
		Paths map[string]struct {
			Post struct {
				RequestBody struct {
					Required bool `json:"required"`
					Content  map[string]struct {
						Schema struct {
							Required   []string `json:"required"`
							Properties map[string]struct {
								Type   string `json:"type"`
								Format string `json:"format"`
							} `json:"properties"`
						} `json:"schema"`
					} `json:"content"`
				} `json:"requestBody"`
			} `json:"post"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	operation, ok := document.Paths["/v1/audio/transcriptions"]
	if !ok {
		t.Fatal("the implemented transcription route is missing from engine discovery")
	}
	body := operation.Post.RequestBody
	schema := body.Content["multipart/form-data"].Schema
	if !body.Required || !reflect.DeepEqual(schema.Required, []string{"model", "file"}) || schema.Properties["model"].Type != "string" || schema.Properties["file"].Type != "string" || schema.Properties["file"].Format != "binary" {
		t.Fatalf("transcription request schema does not match multipart model/file: %+v", body)
	}
	for _, missing := range []string{"", "model", "file"} {
		t.Run("missing="+missing, func(t *testing.T) {
			var payload bytes.Buffer
			form := multipart.NewWriter(&payload)
			if missing != "model" {
				if err := form.WriteField("model", "fake-model"); err != nil {
					t.Fatal(err)
				}
			}
			if missing != "file" {
				file, err := form.CreateFormFile("file", "audio.wav")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte("RIFF-audio")); err != nil {
					t.Fatal(err)
				}
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			response := post(t, server.URL+"/v1/audio/transcriptions", form.FormDataContentType(), &payload)
			defer response.Body.Close()
			result, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			want := http.StatusOK
			if missing != "" {
				want = http.StatusBadRequest
			}
			if response.StatusCode != want || missing == "" && !bytes.Contains(result, []byte(`"text":"hello from fake-engine"`)) {
				t.Fatalf("status=%d body=%s", response.StatusCode, result)
			}
		})
	}
}
