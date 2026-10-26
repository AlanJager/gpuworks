package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestDiscoveryServesCapturedSchemaAndConfiguredVoices(t *testing.T) {
	for _, voices := range [][]string{nil, {"vivian", "ryan"}} {
		handler := New(Options{Voices: voices})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/v1/audio/voices", nil))
		var got struct {
			Voices   []string `json:"voices"`
			Uploaded []string `json:"uploaded_voices"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if response.Code != 200 || !reflect.DeepEqual(got.Voices, append([]string{}, voices...)) || got.Uploaded == nil {
			t.Fatalf("voices=%s status=%d", response.Body, response.Code)
		}
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/openapi.json", nil))
		if response.Code != 200 {
			t.Fatalf("schema status=%d", response.Code)
		}
		var captured, published map[string]any
		if err := json.Unmarshal(openAPIDocument, &captured); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &published); err != nil {
			t.Fatal(err)
		}
		for key, value := range captured {
			if key != "paths" && !reflect.DeepEqual(value, published[key]) {
				t.Fatalf("captured %s changed", key)
			}
		}
		capturedPaths := captured["paths"].(map[string]any)
		publishedPaths := published["paths"].(map[string]any)
		for path, value := range capturedPaths {
			if !reflect.DeepEqual(value, publishedPaths[path]) {
				t.Fatalf("captured route %s changed", path)
			}
		}
		if len(publishedPaths) != len(capturedPaths)+1 {
			t.Fatalf("published %d paths, want captured routes and transcription", len(publishedPaths))
		}
		var schema struct {
			Components struct {
				Schemas map[string]struct {
					Required []string `json:"required"`
				} `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &schema); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(schema.Components.Schemas["OpenAICreateSpeechRequest"].Required, []string{"input"}) {
			t.Fatal("required fields differ from engine")
		}
		for _, path := range []string{"/openapi.json", "/v1/audio/voices", "/v1/realtime"} {
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", path, nil))
			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("POST %s status=%d", path, response.Code)
			}
		}
	}
}

func TestRealtimeHandshakeAndEvents(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "realtime-model"}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, server.URL+"/v1/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatal(err)
	}
	if event["type"] != "session.created" || event["session"].(map[string]any)["model"] != "realtime-model" {
		t.Fatalf("event=%v", event)
	}
	for _, tc := range []struct{ send, want string }{{"unsupported", "error"}, {"session.update", "session.updated"}} {
		if err := wsjson.Write(ctx, conn, map[string]any{"type": tc.send, "session": map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		if err := wsjson.Read(ctx, conn, &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] != tc.want {
			t.Fatalf("event=%v", event)
		}
	}
}
