package nodeaddr

import (
	"strings"
	"testing"
)

func TestHost(t *testing.T) {
	valid := map[string]string{
		"gpu3":             "gpu3",
		"gpu3.example.com": "gpu3.example.com",
		"10.20.30.40":      "10.20.30.40",
		"2001:db8::10":     "2001:db8::10",
		"[2001:db8::10]":   "2001:db8::10",
	}
	for input, want := range valid {
		got, err := Host(input)
		if err != nil || got != want {
			t.Fatalf("Host(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"", " gpu3", "gpu3 ", "gpu 3", "https://gpu3", "gpu3/path",
		"gpu3:19090", "10.20.30.40:19090", "[2001:db8::10]:19090",
		"[gpu3]", "bad_host", "-gpu3", "gpu3..example.com",
	} {
		if _, err := Host(input); err == nil {
			t.Fatalf("Host(%q) succeeded, want error", input)
		}
	}
}

func TestRoutableHost(t *testing.T) {
	valid := map[string]string{
		"10.0.0.8":                  "10.0.0.8",
		"172.16.2.4":                "172.16.2.4",
		"gpu3.internal":             "gpu3.internal",
		"2001:db8::10":              "2001:db8::10",
		"https://gpu3.example.com/": "gpu3.example.com",
		"https://[2001:db8::10]":    "2001:db8::10",
	}
	for _, input := range []string{"0.tcp.ap.ngrok.io:25408", "http://0.tcp.ap.ngrok.io:25408"} {
		if _, err := RoutableHost(input); err == nil {
			t.Fatalf("RoutableHost(%q) accepted a port", input)
		}
		if got, err := RoutableEndpointHost(input); err != nil || got != "0.tcp.ap.ngrok.io" {
			t.Fatalf("RoutableEndpointHost(%q) = %q, %v", input, got, err)
		}
	}
	for input, want := range valid {
		if got, err := RoutableHost(input); err != nil || got != want {
			t.Fatalf("RoutableHost(%q): %v", input, err)
		}
	}
	for _, input := range []string{"localhost", "worker.localhost", "localhost.localdomain", "127.0.0.1", "0.0.0.0", "::1", "::"} {
		if _, err := RoutableHost(input); err == nil {
			t.Fatalf("RoutableHost(%q) succeeded, want error", input)
		}
	}
}

func TestEndpoint(t *testing.T) {
	valid := map[string]string{
		"gpu3":                           "gpu3",
		"gpu3:19090":                     "gpu3:19090",
		"http://0.tcp.ap.ngrok.io:25408": "0.tcp.ap.ngrok.io:25408",
		"https://[2001:db8::10]:19090/":  "[2001:db8::10]:19090",
	}
	for input, want := range valid {
		got, err := Endpoint(input)
		if err != nil || got != want {
			t.Fatalf("Endpoint(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"ftp://gpu3:19090", "http://user@gpu3:19090", "http://gpu3:19090/path",
		"http://gpu3:19090?q=x", "gpu3:bad", "gpu3:70000",
	} {
		if _, err := Endpoint(input); err == nil {
			t.Fatalf("Endpoint(%q) succeeded, want error", input)
		}
	}
}

func TestLocalBaseURL(t *testing.T) {
	// Portability note: this asserts the shape, not a particular address, so it
	// holds on a laptop with no network just as well as on a build machine.
	got := LocalBaseURL(":8800")
	if got != "" {
		if !strings.HasPrefix(got, "http://") || !strings.HasSuffix(got, ":8800") {
			t.Fatalf("LocalBaseURL(:8800) = %q", got)
		}
	}
	for _, bad := range []string{"", "8800", "not-an-addr"} {
		if got := LocalBaseURL(bad); got != "" {
			t.Fatalf("LocalBaseURL(%q) = %q, want empty", bad, got)
		}
	}
}
