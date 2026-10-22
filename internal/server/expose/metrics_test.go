package expose

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The wire shape below is cloudflared's own /config endpoint. The version field
// starts at -1 and only moves once remote configuration has arrived — the
// difference between "the connector is online" and "a route publishes this
// server" is exactly what these fixtures cover.
const metricsConfigFixture = `{
  "version": 3,
  "config": {
    "ingress": [
      {"hostname": "gpu.example.com", "service": "http://localhost:8800", "path": ""},
      {"hostname": "other.example.com", "service": "http://localhost:9999"},
      {"hostname": "*.wild.example.com", "service": "http://localhost:8800"},
      {"hostname": "pathonly.example.com", "service": "http://localhost:8800", "path": "/api"},
      {"service": "http_status:404"}
    ]
  }
}`

func TestSelectHostnames(t *testing.T) {
	cfg := &metricsConfig{}
	if err := unmarshalMetrics(t, metricsConfigFixture, cfg); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		origin string
		want   []string
	}{
		{
			// localhost and 127.0.0.1 are the same server; the operator writes
			// whichever they prefer in the dashboard.
			name: "loopback spelling differs", origin: "http://127.0.0.1:8800",
			want: []string{"gpu.example.com"},
		},
		{
			name: "origin written the same way", origin: "http://localhost:8800",
			want: []string{"gpu.example.com"},
		},
		{
			// A route to another port is another service on the same tunnel.
			name: "another service", origin: "http://127.0.0.1:9999",
			want: []string{"other.example.com"},
		},
		{
			name: "no route points here", origin: "http://127.0.0.1:1234",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectHostnames(cfg, tc.origin); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("selectHostnames = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSelectHostnamesRejectsWildcardPathAndCatchAll(t *testing.T) {
	cfg := &metricsConfig{}
	if err := unmarshalMetrics(t, metricsConfigFixture, cfg); err != nil {
		t.Fatal(err)
	}
	for _, host := range selectHostnames(cfg, "http://localhost:8800") {
		switch host {
		case "*.wild.example.com", "pathonly.example.com":
			t.Fatalf("%s must not be offered as the entry point: a wildcard does not name one site and a path rule does not publish one", host)
		}
	}
}

func TestSelectHostnamesIPv6Loopback(t *testing.T) {
	cfg := &metricsConfig{}
	raw := `{"version":2,"config":{"ingress":[{"hostname":"v6.example.com","service":"http://[::1]:8800"}]}}`
	if err := unmarshalMetrics(t, raw, cfg); err != nil {
		t.Fatal(err)
	}
	if got := selectHostnames(cfg, "http://127.0.0.1:8800"); len(got) != 1 || got[0] != "v6.example.com" {
		t.Fatalf("selectHostnames = %v, want the IPv6 loopback route to match", got)
	}
}

func TestSelectHostnamesSortedAndDeduped(t *testing.T) {
	cfg := &metricsConfig{}
	raw := `{"version":2,"config":{"ingress":[
		{"hostname":"b.example.com","service":"http://localhost:8800"},
		{"hostname":"a.example.com","service":"http://localhost:8800"},
		{"hostname":"b.example.com","service":"http://localhost:8800"}]}}`
	if err := unmarshalMetrics(t, raw, cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{"a.example.com", "b.example.com"}
	if got := selectHostnames(cfg, "http://127.0.0.1:8800"); !reflect.DeepEqual(got, want) {
		t.Fatalf("selectHostnames = %v, want %v", got, want)
	}
}

func TestFetchMetricsConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":-1,"config":{"ingress":[]}}`))
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	cfg, err := fetchMetricsConfig(context.Background(), srv.Client(), addr)
	if err != nil {
		t.Fatalf("fetchMetricsConfig: %v", err)
	}
	if cfg.Version != -1 {
		t.Errorf("version = %d, want -1 (no remote configuration yet)", cfg.Version)
	}

	// A version that answers something else is not usable for discovery, and
	// saying so beats guessing a hostname.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>login</html>"))
	}))
	defer bad.Close()
	if _, err := fetchMetricsConfig(context.Background(), bad.Client(), bad.Listener.Addr().String()); err == nil {
		t.Fatal("want an error for an endpoint that is not the configuration endpoint")
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		service, origin string
		want            bool
	}{
		{"http://localhost:8800", "http://127.0.0.1:8800", true},
		{"http://127.0.0.1:8800", "http://localhost:8800", true},
		{"http://[::1]:8800", "http://127.0.0.1:8800", true},
		{"127.0.0.1:8800", "http://127.0.0.1:8800", true},
		{"http://localhost:8801", "http://127.0.0.1:8800", false},
		{"http://10.0.0.5:8800", "http://127.0.0.1:8800", false},
		{"http://localhost", "http://127.0.0.1:8800", false},
		{"", "http://127.0.0.1:8800", false},
		{"http_status:404", "http://127.0.0.1:8800", false},
	}
	for _, tc := range cases {
		if got := sameOrigin(tc.service, tc.origin); got != tc.want {
			t.Errorf("sameOrigin(%q, %q) = %v, want %v", tc.service, tc.origin, got, tc.want)
		}
	}
}

func TestFreeLoopbackPort(t *testing.T) {
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatalf("freeLoopbackPort: %v", err)
	}
	if port == "" || port == "0" {
		t.Fatalf("port = %q, want a concrete port", port)
	}
}

// unmarshalMetrics decodes a fixture into the endpoint's wire type.
func unmarshalMetrics(t *testing.T, raw string, into *metricsConfig) error {
	t.Helper()
	return json.Unmarshal([]byte(raw), into)
}
